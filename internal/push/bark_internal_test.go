package push

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/sideshow/apns2/payload"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/christianselig/apollo-backend/internal/domain"
)

// The sample payloads mirror the per-category test notifications in
// internal/api/notifications.go, which themselves mirror what the workers
// build in production.

func TestBarkRequestFromPayload_CommentReply(t *testing.T) {
	t.Parallel()

	p := payload.NewPayload().
		MutableContent().
		Sound("traloop.wav").
		AlertTitle("Equinox_Shift in Protests set to disrupt Ottawa's downtown for 3rd straight weekend").
		AlertBody("They don't even go here.").
		Category("inbox-comment-reply").
		Custom("account_id", "1ia22").
		Custom("author", "Equinox_Shift").
		Custom("comment_id", "hwp66zg").
		Custom("post_id", "sqqk29").
		Custom("subreddit", "ottawa").
		Custom("type", "comment").
		ThreadID("comment")

	req, err := barkRequestFromPayload(p)
	require.NoError(t, err)

	assert.Equal(t, "Equinox_Shift in Protests set to disrupt Ottawa's downtown for 3rd straight weekend", req.Title)
	assert.Equal(t, "They don't even go here.", req.Body)
	assert.Equal(t, "apollo://reddit.com/r/ottawa/comments/sqqk29/_/hwp66zg/?context=1", req.URL)
	assert.Equal(t, "comment", req.Group)
	// "traloop.wav" minus the extension: matches assets/bark-sounds/
	// traloop.caf once bark-server appends ".caf".
	assert.Equal(t, "traloop", req.Sound)
}

func TestBarkRequestFromPayload_NoSound(t *testing.T) {
	t.Parallel()

	req, err := barkRequestFromPayload(payload.NewPayload().AlertTitle("Quiet"))
	require.NoError(t, err)
	assert.Empty(t, req.Sound)

	req, err = barkRequestFromPayload(payload.NewPayload().AlertTitle("Stock").Sound("default"))
	require.NoError(t, err)
	assert.Empty(t, req.Sound)
}

func TestBarkRequestFromPayload_PrivateMessage(t *testing.T) {
	t.Parallel()

	p := payload.NewPayload().
		MutableContent().
		Sound("traloop.wav").
		AlertTitle("Message from welcomebot").
		AlertSubtitle("Welcome to r/GriefSupport!").
		AlertBody("**Welcome to r/GriefSupport!**").
		Category("inbox-private-message").
		Custom("account_id", "1ia22").
		Custom("author", "welcomebot").
		Custom("comment_id", "1d2oouy").
		Custom("subreddit", "").
		Custom("type", "private-message")

	req, err := barkRequestFromPayload(p)
	require.NoError(t, err)

	assert.Equal(t, "Message from welcomebot", req.Title)
	assert.Equal(t, "Welcome to r/GriefSupport!", req.Subtitle)
	assert.Equal(t, "apollo://reborn/inbox", req.URL)
	// No thread-id set; category is the grouping fallback.
	assert.Equal(t, "inbox-private-message", req.Group)
}

func TestBarkRequestFromPayload_SubredditWatcher(t *testing.T) {
	t.Parallel()

	p := payload.NewPayload().
		MutableContent().
		Sound("traloop.wav").
		AlertTitle("📣 “bug pics” Watcher").
		AlertBody("r/pics: “A Goliath Stick Insect.”").
		AlertSummaryArg("pics").
		Category("subreddit-watcher").
		Custom("author", "befarked247").
		Custom("post_age", 1651409659.0).
		Custom("post_id", "ufzaml").
		Custom("post_title", "A Goliath Stick Insect.").
		Custom("subreddit", "pics").
		Custom("thumbnail", "https://a.thumbs.redditmedia.com/Lr4b.jpg").
		ThreadID("subreddit-watcher")

	req, err := barkRequestFromPayload(p)
	require.NoError(t, err)

	assert.Equal(t, "📣 “bug pics” Watcher", req.Title)
	assert.Equal(t, "apollo://reddit.com/r/pics/comments/ufzaml", req.URL)
	assert.Equal(t, "subreddit-watcher", req.Group)
	assert.Equal(t, "https://a.thumbs.redditmedia.com/Lr4b.jpg", req.Icon)
}

func TestBarkRequestFromPayload_UsernameMention(t *testing.T) {
	t.Parallel()

	p := payload.NewPayload().
		AlertTitle("Mention in “testimg”").
		AlertBody("yo u/changelog what's good").
		Category("inbox-username-mention-no-context").
		Custom("comment_id", "i6xobpa").
		Custom("post_id", "u02338").
		Custom("subreddit", "calicosummer").
		Custom("type", "username")

	req, err := barkRequestFromPayload(p)
	require.NoError(t, err)

	assert.Equal(t, "apollo://reddit.com/r/calicosummer/comments/u02338/_/i6xobpa/?context=1", req.URL)
}

func TestBarkRequestFromPayload_Badge(t *testing.T) {
	t.Parallel()

	p := payload.NewPayload().
		AlertTitle("Message from someone").
		AlertBody("hi").
		Badge(3).
		Custom("type", "private-message")

	req, err := barkRequestFromPayload(p)
	require.NoError(t, err)

	require.NotNil(t, req.Badge)
	assert.Equal(t, 3, *req.Badge)
}

func TestBarkRequestFromPayload_TestBlastFallsBackToInbox(t *testing.T) {
	t.Parallel()

	// The api's testDeviceHandler payload has no post_id/subreddit customs.
	p := payload.NewPayload().
		Category("test-notification").
		Custom("test_accounts", "changelog").
		AlertTitle("📣 Hello, is this thing on?").
		AlertBody("Active usernames are: changelog. Tap me for more info!").
		MutableContent().
		Sound("traloop.wav")

	req, err := barkRequestFromPayload(p)
	require.NoError(t, err)

	assert.Equal(t, "apollo://reborn/inbox", req.URL)
	assert.Equal(t, "test-notification", req.Group)
}

func TestClickURL_EscapesPathComponents(t *testing.T) {
	t.Parallel()

	got := clickURL(map[string]interface{}{
		"post_id":   "abc123",
		"subreddit": "r weird/name",
	})
	assert.Equal(t, "apollo://reddit.com/r/r%20weird%2Fname/comments/abc123", got)
}

// The slug placeholder must be "_" — Apollo's link parser captures the
// comment id as (\w+) after an optional (?:/\w+)? slug segment, and "-" is
// not a \w character, so a /-/ link silently opens the post unanchored.
func TestClickURL_CommentAnchorUsesUnderscoreSlug(t *testing.T) {
	t.Parallel()

	got := clickURL(map[string]interface{}{
		"post_id":    "1um41tv",
		"subreddit":  "ApolloReborn",
		"comment_id": "ov9d35z",
	})
	assert.Equal(t, "apollo://reddit.com/r/ApolloReborn/comments/1um41tv/_/ov9d35z/?context=1", got)
	assert.NotContains(t, got, "/-/")
}

func TestBarkRequestFromPayload_EmptyBodyFallsBackToTitle(t *testing.T) {
	t.Parallel()

	p := payload.NewPayload().AlertTitle("Only a title")

	req, err := barkRequestFromPayload(p)
	require.NoError(t, err)

	assert.Equal(t, "Only a title", req.Body)
}

type staticBarkResolver map[string][]netip.Addr

func (r staticBarkResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	return r[host], nil
}

type barkResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f barkResolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func newPolicyForTest(t *testing.T, resolver barkResolver, dial func(context.Context, string, string) (net.Conn, error)) *barkDestinationPolicy {
	t.Helper()
	p, err := newBarkDestinationPolicy("https://bark.example", resolver, dial)
	require.NoError(t, err)
	return p
}

func lastAddrInPrefix(prefix netip.Prefix) netip.Addr {
	prefix = prefix.Masked()
	if prefix.Addr().Is4() {
		addr := prefix.Addr().As4()
		for bit := prefix.Bits(); bit < 32; bit++ {
			addr[bit/8] |= byte(1 << uint(7-bit%8))
		}
		return netip.AddrFrom4(addr)
	}

	addr := prefix.Addr().As16()
	for bit := prefix.Bits(); bit < 128; bit++ {
		addr[bit/8] |= byte(1 << uint(7-bit%8))
	}
	return netip.AddrFrom16(addr)
}

func TestIsSafeBarkIP_RejectsEverySpecialUsePrefixBoundary(t *testing.T) {
	t.Parallel()

	// This is deliberately independent of the production table: removing an
	// expected prefix there must make this regression test fail.
	unsafePrefixes := []string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"192.31.196.0/24",
		"192.52.193.0/24",
		"192.88.99.0/24",
		"192.168.0.0/16",
		"192.175.48.0/24",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"::/3",
		"2001::/23",
		"2001:db8::/32",
		"2002::/16",
		"2620:4f:8000::/48",
		"3fff::/20",
		"4000::/2",
		"8000::/1",
	}

	for _, rawPrefix := range unsafePrefixes {
		prefix := netip.MustParsePrefix(rawPrefix)
		for _, addr := range []netip.Addr{prefix.Addr(), lastAddrInPrefix(prefix)} {
			assert.Falsef(t, isSafeBarkIP(addr), "%s boundary %s must be denied", rawPrefix, addr)
		}
	}
}

func TestIsSafeBarkIP_PreservesPublicDestinationsAndUnmapsIPv4(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"1.1.1.1",
		"8.8.8.8",
		"93.184.216.34",
		"2001:4860:4860::8888",
		"2606:4700:4700::1111",
		"::ffff:93.184.216.34",
	} {
		assert.Truef(t, isSafeBarkIP(netip.MustParseAddr(raw)), "%s must remain usable", raw)
	}

	for _, raw := range []string{
		"::ffff:127.0.0.1",
		"::ffff:169.254.169.254",
		"fe80::1%en0",
	} {
		assert.Falsef(t, isSafeBarkIP(netip.MustParseAddr(raw)), "%s must be denied", raw)
	}
	assert.False(t, isSafeBarkIP(netip.Addr{}))
}

func TestBarkPolicyUsesAllowedOriginsEnvironment(t *testing.T) {
	t.Setenv(barkAllowedOriginsEnv, "https://bark.example")

	p, err := newBarkDestinationPolicyFromEnvironment()
	require.NoError(t, err)
	_, ok := p.allowedOrigins["https://bark.example:443"]
	assert.True(t, ok)
}

func TestBarkAllowedOriginErrorsDoNotExposeRawValues(t *testing.T) {
	t.Parallel()

	const sentinel = "BARK-DEVICE-KEY-MUST-NOT-LEAK"
	for _, origins := range []string{
		"https://bark.example/" + sentinel,
		"https://bark.example/" + sentinel + "%zz",
	} {
		_, err := newBarkDestinationPolicy(origins, staticBarkResolver{}, nil)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), sentinel)
	}
}

func TestBarkDestinationPolicy_RejectsUnsafeEndpoints(t *testing.T) {
	t.Parallel()

	p := newPolicyForTest(t, staticBarkResolver{}, nil)
	for _, endpoint := range []string{
		"ftp://bark.example/device",
		"https://user@bark.example/device",
		"https://bark.example:8443/device",
		"https://127.0.0.1/device",
		"https://10.0.0.1/device",
		"https://169.254.169.254/device",
		"https://[::1]/device",
	} {
		endpoint := endpoint
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			assert.Error(t, p.validateEndpoint(endpoint))
		})
	}
}

func TestBarkDestinationPolicy_DialsApprovedPublicAddress(t *testing.T) {
	t.Parallel()

	var dialed string
	p := newPolicyForTest(t, staticBarkResolver{
		"bark.example": {netip.MustParseAddr("93.184.216.34")},
	}, func(_ context.Context, _ string, address string) (net.Conn, error) {
		dialed = address
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	})

	conn, err := p.DialContext(t.Context(), "tcp", "bark.example:443")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	assert.Equal(t, "93.184.216.34:443", dialed)
	require.NoError(t, p.validateEndpoint("https://bark.example/device?icon=alternate"))
}

func TestBarkDestinationPolicy_DNSRebindingToPrivateAddressIsDenied(t *testing.T) {
	t.Parallel()

	dialed := false
	p := newPolicyForTest(t, staticBarkResolver{
		"bark.example": {netip.MustParseAddr("127.0.0.1")},
	}, func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, nil
	})

	_, err := p.DialContext(t.Context(), "tcp", "bark.example:443")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe IP")
	assert.False(t, dialed)
}

func TestBarkDestinationPolicy_MixedDNSAnswerFailsClosed(t *testing.T) {
	t.Parallel()

	dialed := false
	p := newPolicyForTest(t, staticBarkResolver{
		"bark.example": {
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("64:ff9b:1::a00:1"),
		},
	}, func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, nil
	})

	_, err := p.DialContext(t.Context(), "tcp", "bark.example:443")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe IP")
	assert.False(t, dialed)
}

func TestBarkDestinationPolicy_DoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	dialCount := 0
	redirectResponse := "HTTP/1.1 302 Found\r\n" +
		"Location: http://169.254.169.254/latest/meta-data\r\n" +
		"Content-Length: 0\r\n\r\n"
	dial := pipeBarkDialer(redirectResponse)
	p, err := newBarkDestinationPolicy(
		"http://bark.example",
		staticBarkResolver{"bark.example": {netip.MustParseAddr("93.184.216.34")}},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			dialCount++
			return dial(ctx, network, address)
		},
	)
	require.NoError(t, err)

	s := &Sender{barkDefaultIcon: "https://example.com/icon.png"}
	res, err := s.sendBarkWithPolicy(t.Context(), domain.Device{
		Transport:         domain.DeviceTransportBark,
		TransportEndpoint: "http://bark.example/device-key",
	}, payload.NewPayload().AlertTitle("hello"), p)
	require.NoError(t, err)
	assert.False(t, res.Sent)
	assert.Equal(t, http.StatusFound, res.Status)
	assert.Equal(t, 1, dialCount, "the redirect target must never be dialed")
}

func pipeBarkDialer(response string) func(context.Context, string, string) (net.Conn, error) {
	return func(_ context.Context, _ string, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			req, err := http.ReadRequest(bufio.NewReader(server))
			if err == nil {
				_, _ = io.Copy(io.Discard, req.Body)
				_ = req.Body.Close()
			}
			_, _ = io.WriteString(server, response)
		}()
		return client, nil
	}
}

func assertBarkErrorDoesNotExposeEndpoint(t *testing.T, err error, sensitive ...string) {
	t.Helper()
	require.Error(t, err)
	for _, value := range sensitive {
		assert.NotContains(t, err.Error(), value)
	}
}

func TestSendBarkWithPolicy_SanitizesInvalidEndpoint(t *testing.T) {
	t.Parallel()

	const deviceKey = "BARK-DEVICE-KEY-MUST-NOT-LEAK"
	p := newPolicyForTest(t, staticBarkResolver{}, nil)
	s := &Sender{barkDefaultIcon: "https://example.com/icon.png"}

	_, err := s.sendBarkWithPolicy(t.Context(), domain.Device{
		Transport:         domain.DeviceTransportBark,
		TransportEndpoint: "https://bark.example/" + deviceKey + "%zz",
	}, payload.NewPayload().AlertTitle("hello"), p)

	assertBarkErrorDoesNotExposeEndpoint(t, err, deviceKey)
}

func TestSendBarkWithPolicy_SanitizesResolverError(t *testing.T) {
	t.Parallel()

	const deviceKey = "BARK-DEVICE-KEY-MUST-NOT-LEAK"
	const querySecret = "QUERY-SECRET-MUST-NOT-LEAK"
	p, err := newBarkDestinationPolicy(
		"http://bark.example",
		barkResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return nil, errors.New("resolver unavailable")
		}),
		nil,
	)
	require.NoError(t, err)
	s := &Sender{barkDefaultIcon: "https://example.com/icon.png"}

	_, err = s.sendBarkWithPolicy(t.Context(), domain.Device{
		Transport:         domain.DeviceTransportBark,
		TransportEndpoint: "http://bark.example/" + deviceKey + "?token=" + querySecret,
	}, payload.NewPayload().AlertTitle("hello"), p)

	assertBarkErrorDoesNotExposeEndpoint(t, err, deviceKey, querySecret)
}

func TestSendBarkWithPolicy_SanitizesDialError(t *testing.T) {
	t.Parallel()

	const deviceKey = "BARK-DEVICE-KEY-MUST-NOT-LEAK"
	const querySecret = "QUERY-SECRET-MUST-NOT-LEAK"
	p, err := newBarkDestinationPolicy(
		"http://bark.example",
		staticBarkResolver{"bark.example": {netip.MustParseAddr("93.184.216.34")}},
		func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dial unavailable")
		},
	)
	require.NoError(t, err)
	s := &Sender{barkDefaultIcon: "https://example.com/icon.png"}

	_, err = s.sendBarkWithPolicy(t.Context(), domain.Device{
		Transport:         domain.DeviceTransportBark,
		TransportEndpoint: "http://bark.example/" + deviceKey + "?token=" + querySecret,
	}, payload.NewPayload().AlertTitle("hello"), p)

	assertBarkErrorDoesNotExposeEndpoint(t, err, deviceKey, querySecret)
}

func TestSendBarkWithPolicy_SanitizesTLSError(t *testing.T) {
	t.Parallel()

	const deviceKey = "BARK-DEVICE-KEY-MUST-NOT-LEAK"
	const querySecret = "QUERY-SECRET-MUST-NOT-LEAK"
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsServer.Close()

	p, err := newBarkDestinationPolicy(
		"https://bark.example",
		staticBarkResolver{"bark.example": {netip.MustParseAddr("93.184.216.34")}},
		func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, tlsServer.Listener.Addr().String())
		},
	)
	require.NoError(t, err)
	s := &Sender{barkDefaultIcon: "https://example.com/icon.png"}

	_, err = s.sendBarkWithPolicy(t.Context(), domain.Device{
		Transport:         domain.DeviceTransportBark,
		TransportEndpoint: "https://bark.example/" + deviceKey + "?token=" + querySecret,
	}, payload.NewPayload().AlertTitle("hello"), p)

	assertBarkErrorDoesNotExposeEndpoint(t, err, deviceKey, querySecret)
}

func TestSendBarkWithPolicy_SanitizesTimeoutError(t *testing.T) {
	t.Parallel()

	const deviceKey = "BARK-DEVICE-KEY-MUST-NOT-LEAK"
	const querySecret = "QUERY-SECRET-MUST-NOT-LEAK"
	p, err := newBarkDestinationPolicy(
		"http://bark.example",
		staticBarkResolver{"bark.example": {netip.MustParseAddr("93.184.216.34")}},
		func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	require.NoError(t, err)
	s := &Sender{barkDefaultIcon: "https://example.com/icon.png"}
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancel()

	_, err = s.sendBarkWithPolicy(ctx, domain.Device{
		Transport:         domain.DeviceTransportBark,
		TransportEndpoint: "http://bark.example/" + deviceKey + "?token=" + querySecret,
	}, payload.NewPayload().AlertTitle("hello"), p)

	assertBarkErrorDoesNotExposeEndpoint(t, err, deviceKey, querySecret)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestSendBarkWithPolicy_ApprovedOriginSucceeds(t *testing.T) {
	t.Parallel()

	p, err := newBarkDestinationPolicy(
		"http://bark.example",
		staticBarkResolver{"bark.example": {netip.MustParseAddr("93.184.216.34")}},
		pipeBarkDialer("HTTP/1.1 200 OK\r\nContent-Length: 32\r\n\r\n{\"code\":200,\"message\":\"success\"}"),
	)
	require.NoError(t, err)

	s := &Sender{barkDefaultIcon: "https://example.com/icon.png"}
	res, err := s.sendBarkWithPolicy(t.Context(), domain.Device{
		Transport:         domain.DeviceTransportBark,
		TransportEndpoint: "http://bark.example/device-key?icon=alternate",
	}, payload.NewPayload().AlertTitle("hello"), p)
	require.NoError(t, err)
	assert.True(t, res.Sent)
}

func TestSendBarkWithPolicy_DoesNotExposeEndpointResponseBody(t *testing.T) {
	t.Parallel()

	const deviceKey = "BARK-DEVICE-KEY-MUST-NOT-LEAK"
	const querySecret = "QUERY-SECRET-MUST-NOT-LEAK"
	const responseSecret = "RESPONSE-SECRET-MUST-NOT-LEAK"
	p, err := newBarkDestinationPolicy(
		"http://bark.example",
		staticBarkResolver{"bark.example": {netip.MustParseAddr("93.184.216.34")}},
		pipeBarkDialer("HTTP/1.1 200 OK\r\nContent-Length: 54\r\n\r\n{\"code\":403,\"message\":\""+responseSecret+"\"}"),
	)
	require.NoError(t, err)

	s := &Sender{barkDefaultIcon: "https://example.com/icon.png"}
	res, err := s.sendBarkWithPolicy(t.Context(), domain.Device{
		Transport:         domain.DeviceTransportBark,
		TransportEndpoint: "http://bark.example/" + deviceKey + "?token=" + querySecret,
	}, payload.NewPayload().AlertTitle("hello"), p)
	require.NoError(t, err)
	assert.False(t, res.Sent)
	assert.Equal(t, "Bark endpoint rejected the notification", res.Reason)
	assert.NotContains(t, res.Reason, deviceKey)
	assert.NotContains(t, res.Reason, querySecret)
	assert.NotContains(t, res.Reason, responseSecret)
}
