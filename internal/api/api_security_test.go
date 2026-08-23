package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const testRegistrationSecret = "0123456789abcdef0123456789abcdef"

func newSecurityTestAPI(logger *zap.Logger) *api {
	return &api{
		logger: logger,
		statsd: &statsd.NoOpClient{},
	}
}

func authenticatedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("X-Registration-Token", testRegistrationSecret)
	return req
}

func TestAllNonHealthRoutesRequireRegistrationToken(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")
	h := newSecurityTestAPI(zap.NewNop()).Routes()

	tests := []struct {
		name   string
		method string
		target string
	}{
		{"diagnostic", http.MethodGet, "/api/announcement"},
		{"receipt", http.MethodPost, "/v1/receipt"},
		{"device deletion", http.MethodDelete, "/v1/device/device-token"},
		{"test push", http.MethodPost, "/v1/device/device-token/test"},
		{"account preferences", http.MethodPatch, "/v1/device/device-token/account/reddit-id/notifications"},
		{"watcher list", http.MethodGet, "/v1/device/device-token/account/reddit-id/watchers"},
		{"method mismatch", http.MethodPost, "/api/announcement"},
		{"unmatched route", http.MethodPost, "/not-a-route"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(tt.method, tt.target, nil))
			require.Equal(t, http.StatusUnauthorized, rr.Code)
		})
	}

	health := httptest.NewRecorder()
	h.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	require.Equal(t, http.StatusOK, health.Code)

	announcement := httptest.NewRecorder()
	h.ServeHTTP(announcement, authenticatedRequest(http.MethodGet, "/api/announcement", nil))
	require.Equal(t, http.StatusOK, announcement.Code)
}

func TestMissingSecretFailsClosedExceptExplicitDevelopmentBypass(t *testing.T) {
	t.Setenv(registrationSecretEnv, "")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")
	t.Setenv("ENV", "production")
	require.Error(t, ValidateConfiguration())

	h := newSecurityTestAPI(zap.NewNop()).Routes()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/announcement", nil))
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)

	t.Setenv(registrationSecretEnv, "too-short")
	require.Error(t, ValidateConfiguration())
	h = newSecurityTestAPI(zap.NewNop()).Routes()
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/announcement", nil))
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)

	t.Setenv("ENV", "development")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "1")
	require.Error(t, ValidateConfiguration())
	h = newSecurityTestAPI(zap.NewNop()).Routes()
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/announcement", nil))
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)

	t.Setenv(registrationSecretEnv, "")
	require.NoError(t, ValidateConfiguration())
	h = newSecurityTestAPI(zap.NewNop()).Routes()
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/announcement", nil))
	require.Equal(t, http.StatusOK, rr.Code)
}

func TestBodyLimitAppliesBeforeDiagnosticHandler(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")
	h := newSecurityTestAPI(zap.NewNop()).Routes()

	tooLarge := bytes.NewReader(bytes.Repeat([]byte("a"), maxRequestBodyBytes+1))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authenticatedRequest(http.MethodPost, "/api/req_v2", tooLarge))
	require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
}

func TestRequestLoggingUsesRouteTemplateAndDirectPeer(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")
	core, logs := observer.New(zap.InfoLevel)
	h := newSecurityTestAPI(zap.New(core)).Routes()

	req := authenticatedRequest(http.MethodGet, "/api/announcement?device_token=do-not-log", nil)
	req.RemoteAddr = "198.51.100.9:443"
	req.Header.Set("X-Forwarded-For", "untrusted-client")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	entries := logs.All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "/api/announcement", fields["route"])
	require.Equal(t, "198.51.100.9", fields["peer#addr"])
	require.NotContains(t, fields, "uri")
	require.NotContains(t, fields, "remote#addr")
}

func TestUnmatchedRequestDoesNotLogRawPathOrBody(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")
	core, logs := observer.New(zap.InfoLevel)
	h := newSecurityTestAPI(zap.New(core)).Routes()

	const sensitive = "sensitive-device-token-and-body"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authenticatedRequest(http.MethodPost, "/unknown?token="+sensitive, bytes.NewReader([]byte(sensitive))))
	require.Equal(t, http.StatusNotFound, rr.Code)

	for _, entry := range logs.All() {
		require.NotContains(t, fmt.Sprint(entry.ContextMap()), sensitive)
	}
}

func TestServerHasInboundLimits(t *testing.T) {
	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	t.Setenv("ENV", "production")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "")
	s := newSecurityTestAPI(zap.NewNop()).Server(4000)
	require.Equal(t, ":4000", s.Addr)
	require.Equal(t, 5*time.Second, s.ReadHeaderTimeout)
	require.Equal(t, 15*time.Second, s.ReadTimeout)
	require.Equal(t, 60*time.Second, s.WriteTimeout)
	require.Equal(t, 120*time.Second, s.IdleTimeout)
	require.Equal(t, maxRequestHeaderBytes, s.MaxHeaderBytes)
}

func TestUnsafeDevelopmentBypassBindsLoopback(t *testing.T) {
	t.Setenv(registrationSecretEnv, "")
	t.Setenv("ENV", "development")
	t.Setenv(unsafeDevelopmentAuthBypassEnv, "1")
	require.Equal(t, "127.0.0.1:4000", newSecurityTestAPI(zap.NewNop()).Server(4000).Addr)

	t.Setenv(registrationSecretEnv, testRegistrationSecret)
	require.Equal(t, ":4000", newSecurityTestAPI(zap.NewNop()).Server(4000).Addr)
}
