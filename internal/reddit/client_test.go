package reddit_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/christianselig/apollo-backend/internal/reddit"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAuthenticatedClientObfuscatedToken(t *testing.T) {
	t.Parallel()

	tracer := otel.Tracer("test")
	rc := reddit.NewClient(tracer, nil, nil, 1)

	type test struct {
		have string
		want string
	}

	tests := []test{
		{"abc", "<SHORT>"},
		{"abcdefghi", "abc...ghi"},
	}

	for _, tc := range tests {
		rac := rc.NewAuthenticatedClient(reddit.AuthCredentials{
			RedditID:     "<ID>",
			RefreshToken: "<REFRESH>",
			AccessToken:  tc.have,
			ClientID:     "<CLIENT>",
			ClientSecret: "<SECRET>",
			UserAgent:    "test/1.0",
		})
		got := rac.ObfuscatedAccessToken()

		assert.Equal(t, tc.want, got)
	}
}

func TestMeWithAccessTokenUsesFixedEndpointAndCallerToken(t *testing.T) {
	t.Parallel()

	const accessToken = "test-caller-access-token"
	calls := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "https", req.URL.Scheme)
		require.Equal(t, "oauth.reddit.com", req.URL.Host)
		require.Equal(t, "/api/v1/me", req.URL.Path)
		require.Equal(t, "1", req.URL.Query().Get("raw_json"))
		require.Equal(t, "Bearer "+accessToken, req.Header.Get("Authorization"))
		require.NotEmpty(t, req.Header.Get("User-Agent"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"abc123","name":"ApolloUser"}`)),
			Request:    req,
		}, nil
	})}
	t.Cleanup(httpClient.CloseIdleConnections)

	rc := reddit.NewClient(otel.Tracer("test"), &statsd.NoOpClient{}, nil, 1)
	me, err := rc.MeWithAccessToken(
		context.Background(),
		accessToken,
		reddit.WithClient(httpClient),
		reddit.WithRetry(false),
	)
	require.NoError(t, err)
	require.Equal(t, "abc123", me.ID)
	require.Equal(t, "ApolloUser", me.Name)
	require.Equal(t, 1, calls)
}
