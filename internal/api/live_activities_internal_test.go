package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DataDog/datadog-go/statsd"
	apnstoken "github.com/sideshow/apns2/token"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/christianselig/apollo-backend/internal/domain"
	"github.com/christianselig/apollo-backend/internal/reddit"
)

type liveActivityAccountRepo struct {
	domain.AccountRepository
	account domain.Account
	err     error
}

func (r *liveActivityAccountRepo) GetByRedditID(_ context.Context, _ string) (domain.Account, error) {
	return r.account, r.err
}

type liveActivityRepo struct {
	domain.LiveActivityRepository
	created []*domain.LiveActivity
}

func (r *liveActivityRepo) Create(_ context.Context, activity *domain.LiveActivity) error {
	copy := *activity
	r.created = append(r.created, &copy)
	return nil
}

type fakeRedditIdentityVerifier struct {
	identity  *reddit.MeResponse
	err       error
	calls     int
	seenToken string
}

func (v *fakeRedditIdentityVerifier) MeWithAccessToken(_ context.Context, accessToken string, _ ...reddit.RequestOption) (*reddit.MeResponse, error) {
	v.calls++
	v.seenToken = accessToken
	return v.identity, v.err
}

func liveActivityPayload(t *testing.T, redditID, accessToken string) *bytes.Reader {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"apns_token":        strings.Repeat("a", 64),
		"reddit_account_id": redditID,
		"access_token":      accessToken,
		"refresh_token":     "unused-refresh-token",
		"thread_id":         "thread123",
		"subreddit":         "apolloapp",
	})
	require.NoError(t, err)
	return bytes.NewReader(payload)
}

func TestCreateLiveActivityProvesRedditAccountOwnership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		requestID    string
		accessToken  string
		identity     *reddit.MeResponse
		identityErr  error
		noVerifier   bool
		wantStatus   int
		wantCreate   bool
		wantVerifier bool
	}{
		{
			name:         "matching bare account ID",
			requestID:    "abc123",
			accessToken:  "caller-access-token",
			identity:     &reddit.MeResponse{ID: "abc123"},
			wantStatus:   http.StatusOK,
			wantCreate:   true,
			wantVerifier: true,
		},
		{
			name:         "t2 fullname normalizes",
			requestID:    "t2_abc123",
			accessToken:  "caller-access-token",
			identity:     &reddit.MeResponse{ID: "abc123"},
			wantStatus:   http.StatusOK,
			wantCreate:   true,
			wantVerifier: true,
		},
		{
			name:         "identity mismatch fails closed",
			requestID:    "abc123",
			accessToken:  "caller-access-token",
			identity:     &reddit.MeResponse{ID: "different"},
			wantStatus:   http.StatusUnauthorized,
			wantVerifier: true,
		},
		{
			name:        "missing access token fails closed",
			requestID:   "abc123",
			accessToken: "",
			identity:    &reddit.MeResponse{ID: "abc123"},
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name:         "revoked token fails closed",
			requestID:    "abc123",
			accessToken:  "revoked-access-token",
			identityErr:  reddit.ErrOauthRevoked,
			wantStatus:   http.StatusUnauthorized,
			wantVerifier: true,
		},
		{
			name:         "Reddit transport failure fails closed",
			requestID:    "abc123",
			accessToken:  "caller-access-token",
			identityErr:  errors.New("upstream unavailable"),
			wantStatus:   http.StatusBadGateway,
			wantVerifier: true,
		},
		{
			name:        "missing verifier fails closed",
			requestID:   "abc123",
			accessToken: "caller-access-token",
			noVerifier:  true,
			wantStatus:  http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			accountRepo := &liveActivityAccountRepo{account: domain.Account{AccountID: "abc123"}}
			activityRepo := &liveActivityRepo{}
			verifier := &fakeRedditIdentityVerifier{identity: tc.identity, err: tc.identityErr}
			var identityVerifier redditIdentityVerifier = verifier
			if tc.noVerifier {
				identityVerifier = nil
			}

			a := &api{
				logger:           zap.NewNop(),
				statsd:           &statsd.NoOpClient{},
				apns:             &apnstoken.Token{},
				accountRepo:      accountRepo,
				liveActivityRepo: activityRepo,
				redditIdentity:   identityVerifier,
			}

			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/live_activities", liveActivityPayload(t, tc.requestID, tc.accessToken))
			a.createLiveActivityHandler(rr, req)

			require.Equal(t, tc.wantStatus, rr.Code)
			if tc.wantCreate {
				require.Len(t, activityRepo.created, 1)
				require.Equal(t, "abc123", activityRepo.created[0].RedditAccountID)
			} else {
				require.Empty(t, activityRepo.created)
			}
			if tc.wantVerifier {
				require.Equal(t, 1, verifier.calls)
				require.Equal(t, tc.accessToken, verifier.seenToken)
			} else {
				require.Zero(t, verifier.calls)
			}
			if tc.accessToken != "" {
				require.NotContains(t, rr.Body.String(), tc.accessToken)
			}
		})
	}
}
