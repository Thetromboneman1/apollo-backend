package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/christianselig/apollo-backend/internal/domain"
)

const (
	compatDeviceToken = "sensitive-device-token"
	compatAccountID   = "resolved-account-id"
)

type accountCompatAccountRepo struct {
	domain.AccountRepository
	accounts          []domain.Account
	getByAPNSErr      error
	getByAPNSCalls    int
	getByRedditCalls  int
	requestedAPNS     string
	requestedRedditID string
}

func (r *accountCompatAccountRepo) GetByAPNSToken(_ context.Context, token string) ([]domain.Account, error) {
	r.getByAPNSCalls++
	r.requestedAPNS = token
	return r.accounts, r.getByAPNSErr
}

func (r *accountCompatAccountRepo) GetByRedditID(_ context.Context, id string) (domain.Account, error) {
	r.getByRedditCalls++
	r.requestedRedditID = id
	return domain.Account{ID: 11, AccountID: id}, nil
}

type accountCompatDeviceRepo struct {
	domain.DeviceRepository
	getCalls             int
	getNotifiableCalls   int
	setNotifiableCalls   int
	requestedAccountID   string
	requestedDeviceToken string
}

func (r *accountCompatDeviceRepo) GetByAPNSToken(_ context.Context, token string) (domain.Device, error) {
	r.getCalls++
	r.requestedDeviceToken = token
	return domain.Device{ID: 7, APNSToken: token}, nil
}

func (r *accountCompatDeviceRepo) GetNotifiable(_ context.Context, _ *domain.Device, account *domain.Account) (bool, bool, bool, error) {
	r.getNotifiableCalls++
	r.requestedAccountID = account.AccountID
	return true, false, false, nil
}

func (r *accountCompatDeviceRepo) SetNotifiable(_ context.Context, _ *domain.Device, account *domain.Account, _, _, _ bool) error {
	r.setNotifiableCalls++
	r.requestedAccountID = account.AccountID
	return nil
}

type accountCompatWatcherRepo struct {
	domain.WatcherRepository
	watcher              domain.Watcher
	getByIDCalls         int
	listCalls            int
	createCalls          int
	updateCalls          int
	deleteCalls          int
	updateAssociationErr error
	deleteAssociationErr error
	deleteErr            error
	requestedRedditID    string
}

func (r *accountCompatWatcherRepo) GetByID(_ context.Context, _ int64) (domain.Watcher, error) {
	r.getByIDCalls++
	return r.watcher, nil
}

func (r *accountCompatWatcherRepo) GetByDeviceAPNSTokenAndAccountRedditID(_ context.Context, _ string, redditID string) ([]domain.Watcher, error) {
	r.listCalls++
	r.requestedRedditID = redditID
	return []domain.Watcher{}, nil
}

func (r *accountCompatWatcherRepo) Create(_ context.Context, _ *domain.Watcher) error {
	r.createCalls++
	return nil
}

func (r *accountCompatWatcherRepo) Update(_ context.Context, _ *domain.Watcher) error {
	r.updateCalls++
	return nil
}

func (r *accountCompatWatcherRepo) UpdateForDeviceAndAccount(_ context.Context, _ *domain.Watcher, _ string, redditID string) error {
	r.updateCalls++
	r.requestedRedditID = redditID
	return r.updateAssociationErr
}

func (r *accountCompatWatcherRepo) Delete(_ context.Context, _ int64) error {
	r.deleteCalls++
	return r.deleteErr
}

func (r *accountCompatWatcherRepo) DeleteForDeviceAndAccount(_ context.Context, _ int64, _ string, redditID string) error {
	r.deleteCalls++
	r.requestedRedditID = redditID
	if r.deleteAssociationErr != nil {
		return r.deleteAssociationErr
	}
	return r.deleteErr
}

func newAccountCompatTestAPI(logger *zap.Logger, accounts []domain.Account) (*api, *accountCompatAccountRepo, *accountCompatDeviceRepo, *accountCompatWatcherRepo) {
	accountRepo := &accountCompatAccountRepo{accounts: accounts}
	deviceRepo := &accountCompatDeviceRepo{}
	watcherRepo := &accountCompatWatcherRepo{
		watcher: domain.Watcher{
			ID:      17,
			Type:    domain.UserWatcher,
			Device:  domain.Device{APNSToken: compatDeviceToken},
			Account: domain.Account{AccountID: compatAccountID},
		},
	}
	return &api{
		logger:      logger,
		statsd:      &statsd.NoOpClient{},
		accountRepo: accountRepo,
		deviceRepo:  deviceRepo,
		watcherRepo: watcherRepo,
	}, accountRepo, deviceRepo, watcherRepo
}

func accountCompatRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, body)
	req.Header.Set("X-Registration-Token", testRegistrationSecret)
	return req
}

func TestEmptyAccountCompatibilityRoutesRequireAuthentication(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"get notifications", http.MethodGet, "/v1/device/token/account//notifications", ""},
		{"patch notifications", http.MethodPatch, "/v1/device/token/account//notifications", "{}"},
		{"list watchers", http.MethodGet, "/v1/device/token/account//watchers", ""},
		{"create watcher", http.MethodPost, "/v1/device/token/account//watcher", "{}"},
		{"edit watcher", http.MethodPatch, "/v1/device/token/account//watcher/17", "{}"},
		{"delete watcher", http.MethodDelete, "/v1/device/token/account//watcher/17", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, accounts, devices, watchers := newAccountCompatTestAPI(zap.NewNop(), []domain.Account{{AccountID: compatAccountID}})
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body)))

			require.Equal(t, http.StatusUnauthorized, rr.Code)
			require.Zero(t, accounts.getByAPNSCalls+accounts.getByRedditCalls)
			require.Zero(t, devices.getCalls)
			require.Zero(t, watchers.getByIDCalls+watchers.listCalls+watchers.createCalls+watchers.updateCalls+watchers.deleteCalls)
		})
	}
}

func TestEmptyAccountCompatibilityRoutesDelegateWithResolvedAccount(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		assertCall func(*testing.T, *accountCompatAccountRepo, *accountCompatDeviceRepo, *accountCompatWatcherRepo)
	}{
		{
			name:       "get notifications",
			method:     http.MethodGet,
			path:       "/v1/device/" + compatDeviceToken + "/account//notifications?redditID=attacker",
			wantStatus: http.StatusOK,
			assertCall: func(t *testing.T, accounts *accountCompatAccountRepo, devices *accountCompatDeviceRepo, _ *accountCompatWatcherRepo) {
				t.Helper()

				require.Equal(t, 1, accounts.getByAPNSCalls)
				require.Equal(t, 1, accounts.getByRedditCalls)
				require.Equal(t, compatAccountID, accounts.requestedRedditID)
				require.Equal(t, compatAccountID, devices.requestedAccountID)
			},
		},
		{
			name:       "patch notifications",
			method:     http.MethodPatch,
			path:       "/v1/device/" + compatDeviceToken + "/account//notifications",
			body:       `{}`,
			wantStatus: http.StatusOK,
			assertCall: func(t *testing.T, accounts *accountCompatAccountRepo, devices *accountCompatDeviceRepo, _ *accountCompatWatcherRepo) {
				t.Helper()

				require.Equal(t, compatAccountID, accounts.requestedRedditID)
				require.Equal(t, 1, devices.setNotifiableCalls)
			},
		},
		{
			name:       "list watchers",
			method:     http.MethodGet,
			path:       "/v1/device/" + compatDeviceToken + "/account//watchers",
			wantStatus: http.StatusOK,
			assertCall: func(t *testing.T, _ *accountCompatAccountRepo, _ *accountCompatDeviceRepo, watchers *accountCompatWatcherRepo) {
				t.Helper()

				require.Equal(t, 1, watchers.listCalls)
				require.Equal(t, compatAccountID, watchers.requestedRedditID)
			},
		},
		{
			name:       "create watcher",
			method:     http.MethodPost,
			path:       "/v1/device/" + compatDeviceToken + "/account//watcher",
			body:       `{"type":"unsupported"}`,
			wantStatus: http.StatusUnprocessableEntity,
			assertCall: func(t *testing.T, accounts *accountCompatAccountRepo, devices *accountCompatDeviceRepo, watchers *accountCompatWatcherRepo) {
				t.Helper()

				require.Equal(t, 2, accounts.getByAPNSCalls)
				require.Equal(t, 1, devices.getCalls)
				require.Zero(t, watchers.createCalls)
			},
		},
		{
			name:       "edit watcher",
			method:     http.MethodPatch,
			path:       "/v1/device/" + compatDeviceToken + "/account//watcher/17",
			body:       `{}`,
			wantStatus: http.StatusOK,
			assertCall: func(t *testing.T, _ *accountCompatAccountRepo, _ *accountCompatDeviceRepo, watchers *accountCompatWatcherRepo) {
				t.Helper()

				require.Equal(t, 1, watchers.updateCalls)
			},
		},
		{
			name:       "delete watcher",
			method:     http.MethodDelete,
			path:       "/v1/device/" + compatDeviceToken + "/account//watcher/17",
			wantStatus: http.StatusOK,
			assertCall: func(t *testing.T, _ *accountCompatAccountRepo, _ *accountCompatDeviceRepo, watchers *accountCompatWatcherRepo) {
				t.Helper()

				require.Equal(t, 1, watchers.deleteCalls)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, accounts, devices, watchers := newAccountCompatTestAPI(zap.NewNop(), []domain.Account{{AccountID: compatAccountID}})
			rr := httptest.NewRecorder()
			req := accountCompatRequest(t, tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("X-Apollo-Reddit-ID", "attacker")
			a.Routes().ServeHTTP(rr, req)

			require.Equal(t, tc.wantStatus, rr.Code, rr.Body.String())
			require.Equal(t, compatDeviceToken, accounts.requestedAPNS)
			tc.assertCall(t, accounts, devices, watchers)
		})
	}
}

func TestEmptyAccountCompatibilityResolutionFailsClosed(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")

	const repositorySecret = "repository-error-with-sensitive-identifiers"
	tests := []struct {
		name       string
		accounts   []domain.Account
		repoErr    error
		wantStatus int
	}{
		{"no account", nil, nil, http.StatusNotFound},
		{"ambiguous accounts", []domain.Account{{AccountID: "secret-one"}, {AccountID: "secret-two"}}, nil, http.StatusConflict},
		{"repository error", nil, errors.New(repositorySecret), http.StatusInternalServerError},
		{"empty stored account ID", []domain.Account{{AccountID: ""}}, nil, http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			core, logs := observer.New(zap.DebugLevel)
			a, accounts, devices, watchers := newAccountCompatTestAPI(zap.New(core), tc.accounts)
			accounts.getByAPNSErr = tc.repoErr
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, accountCompatRequest(t, http.MethodGet, "/v1/device/"+compatDeviceToken+"/account//notifications", nil))

			require.Equal(t, tc.wantStatus, rr.Code)
			require.Equal(t, 1, accounts.getByAPNSCalls)
			require.Zero(t, accounts.getByRedditCalls)
			require.Zero(t, devices.getCalls)
			require.Zero(t, watchers.listCalls+watchers.createCalls+watchers.updateCalls+watchers.deleteCalls)

			visible := rr.Body.String() + rr.Header().Get("X-Apollo-Error")
			require.NotContains(t, visible, repositorySecret)
			require.NotContains(t, visible, compatDeviceToken)
			require.NotContains(t, visible, "secret-one")
			require.NotContains(t, visible, "secret-two")
			for _, entry := range logs.All() {
				logged := entry.Message + fmt.Sprint(entry.ContextMap())
				require.NotContains(t, logged, repositorySecret)
				require.NotContains(t, logged, compatDeviceToken)
				require.NotContains(t, logged, "secret-one")
				require.NotContains(t, logged, "secret-two")
			}
		})
	}
}

func TestAmbiguousEmptyAccountNeverMutates(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")

	tests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPatch, "/v1/device/token/account//notifications", `{}`},
		{http.MethodPost, "/v1/device/token/account//watcher", `{"type":"unsupported"}`},
		{http.MethodPatch, "/v1/device/token/account//watcher/17", `{}`},
		{http.MethodDelete, "/v1/device/token/account//watcher/17", ""},
	}

	for _, tc := range tests {
		a, accounts, devices, watchers := newAccountCompatTestAPI(zap.NewNop(), []domain.Account{{AccountID: "one"}, {AccountID: "two"}})
		rr := httptest.NewRecorder()
		a.Routes().ServeHTTP(rr, accountCompatRequest(t, tc.method, tc.path, strings.NewReader(tc.body)))

		require.Equal(t, http.StatusConflict, rr.Code)
		require.Equal(t, 1, accounts.getByAPNSCalls)
		require.Zero(t, devices.getCalls+devices.setNotifiableCalls)
		require.Zero(t, watchers.getByIDCalls+watchers.createCalls+watchers.updateCalls+watchers.deleteCalls)
	}
}

func TestEmptyAccountCompatibilityRejectsNonCanonicalPaths(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"wrong method", http.MethodPost, "/v1/device/token/account//notifications"},
		{"triple slash", http.MethodGet, "/v1/device/token/account///notifications"},
		{"trailing slash", http.MethodGet, "/v1/device/token/account//notifications/"},
		{"encoded slash", http.MethodGet, "/v1/device/token/account/%2Fnotifications"},
		{"double encoded slash", http.MethodGet, "/v1/device/token/account/%252Fnotifications"},
		{"encoded backslash", http.MethodGet, "/v1/device/token/account/%5Cnotifications"},
		{"unrelated", http.MethodGet, "/v1/device/token/account//unrelated"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, accounts, _, _ := newAccountCompatTestAPI(zap.NewNop(), []domain.Account{{AccountID: compatAccountID}})
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, accountCompatRequest(t, tc.method, tc.path, nil))

			require.Equal(t, http.StatusNotFound, rr.Code)
			require.Empty(t, rr.Header().Get("Location"))
			require.Zero(t, accounts.getByAPNSCalls)
		})
	}
}

func TestEmptyAccountCompatibilityBodyLimitRunsBeforeResolver(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")

	for _, endpoint := range []struct {
		method string
		path   string
	}{
		{http.MethodPatch, "/v1/device/token/account//notifications"},
		{http.MethodPost, "/v1/device/token/account//watcher"},
		{http.MethodPatch, "/v1/device/token/account//watcher/17"},
	} {
		a, accounts, _, _ := newAccountCompatTestAPI(zap.NewNop(), []domain.Account{{AccountID: compatAccountID}})
		rr := httptest.NewRecorder()
		body := bytes.NewReader(bytes.Repeat([]byte("a"), maxRequestBodyBytes+1))
		a.Routes().ServeHTTP(rr, accountCompatRequest(t, endpoint.method, endpoint.path, body))

		require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
		require.Zero(t, accounts.getByAPNSCalls)
	}
}

func TestEmptyAccountCompatibilityLogsOnlyRouteTemplate(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")

	core, logs := observer.New(zap.InfoLevel)
	a, _, _, _ := newAccountCompatTestAPI(zap.New(core), []domain.Account{{AccountID: compatAccountID}})
	rr := httptest.NewRecorder()
	a.Routes().ServeHTTP(rr, accountCompatRequest(t, http.MethodGet, "/v1/device/"+compatDeviceToken+"/account//notifications?account="+compatAccountID, nil))
	require.Equal(t, http.StatusOK, rr.Code)

	require.Len(t, logs.All(), 1)
	entry := logs.All()[0]
	require.Equal(t, "/v1/device/{apns}/account//notifications", entry.ContextMap()["route"])
	logged := entry.Message + fmt.Sprint(entry.ContextMap())
	require.NotContains(t, logged, compatDeviceToken)
	require.NotContains(t, logged, compatAccountID)
}

func TestWatcherMutationRejectsWrongOwner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		method      string
		call        func(*api, http.ResponseWriter, *http.Request)
		wrongDevice bool
	}{
		{"edit wrong account", http.MethodPatch, (*api).editWatcherHandler, false},
		{"edit wrong device", http.MethodPatch, (*api).editWatcherHandler, true},
		{"delete wrong account", http.MethodDelete, (*api).deleteWatcherHandler, false},
		{"delete wrong device", http.MethodDelete, (*api).deleteWatcherHandler, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, _, _, watchers := newAccountCompatTestAPI(zap.NewNop(), nil)
			if tc.wrongDevice {
				watchers.watcher.Device.APNSToken = "different-device"
			} else {
				watchers.watcher.Account.AccountID = "different-account"
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), tc.method, "/", strings.NewReader("not-json"))
			req = mux.SetURLVars(req, map[string]string{
				"apns":      compatDeviceToken,
				"redditID":  compatAccountID,
				"watcherID": "17",
			})
			tc.call(a, rr, req)

			require.Equal(t, http.StatusNotFound, rr.Code)
			require.Zero(t, watchers.updateCalls+watchers.deleteCalls)
		})
	}
}

func TestDeleteWatcherRepositoryErrorIsSanitized(t *testing.T) {
	t.Parallel()

	a, _, _, watchers := newAccountCompatTestAPI(zap.NewNop(), nil)
	const sensitive = "database error with request identifiers"
	watchers.deleteErr = errors.New(sensitive)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/", nil)
	req = mux.SetURLVars(req, map[string]string{
		"apns":      compatDeviceToken,
		"redditID":  compatAccountID,
		"watcherID": "17",
	})
	a.deleteWatcherHandler(rr, req)

	require.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Equal(t, 1, watchers.deleteCalls)
	require.NotContains(t, rr.Body.String(), sensitive)
	require.NotContains(t, rr.Header().Get("X-Apollo-Error"), sensitive)
}

func TestWatcherMutationRejectsStaleAccountAssociation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		call   func(*api, http.ResponseWriter, *http.Request)
	}{
		{"edit", http.MethodPatch, (*api).editWatcherHandler},
		{"delete", http.MethodDelete, (*api).deleteWatcherHandler},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, _, _, watchers := newAccountCompatTestAPI(zap.NewNop(), nil)
			watchers.updateAssociationErr = domain.ErrNotFound
			watchers.deleteAssociationErr = domain.ErrNotFound
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), tc.method, "/", strings.NewReader(`{}`))
			req = mux.SetURLVars(req, map[string]string{
				"apns":      compatDeviceToken,
				"redditID":  compatAccountID,
				"watcherID": "17",
			})
			tc.call(a, rr, req)

			require.Equal(t, http.StatusNotFound, rr.Code)
			require.Equal(t, 1, watchers.updateCalls+watchers.deleteCalls)
			require.Equal(t, compatAccountID, watchers.requestedRedditID)
		})
	}
}

func TestOriginGatewayPreservesDoubleSlashCompatibilityPaths(t *testing.T) {
	t.Parallel()

	configuration, err := os.ReadFile("../../docs/deployment/nginx.conf")
	require.NoError(t, err)

	found := false
	for _, line := range strings.Split(string(configuration), "\n") {
		if strings.TrimSpace(line) == "merge_slashes off;" {
			found = true
			break
		}
	}
	require.True(t, found, "Nginx must preserve the empty account path's double slash")
}
