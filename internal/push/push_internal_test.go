package push

import (
	"net/netip"
	"testing"

	"github.com/sideshow/apns2/payload"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/christianselig/apollo-backend/internal/domain"
)

// Bark-only mode: NewSender must tolerate a nil APNs token (cmdutil.LoadAPNS
// returns one when no APPLE_* vars are set) without panicking, and APNs sends
// must fail with a plain error — critically, NOT ShouldUnregister, which
// would make the notifications worker delete the device row.

func TestNewSender_NilTokenBarkOnly(t *testing.T) {
	t.Parallel()

	s := NewSender(zap.NewNop(), nil, "")
	require.NotNil(t, s)
	assert.Nil(t, s.apnsProd)
	assert.Nil(t, s.apnsSandbox)
}

func TestSend_BarkWorksWithNilAPNSTokenAndApprovedOrigin(t *testing.T) {
	t.Parallel()

	p, err := newBarkDestinationPolicy(
		"http://bark.example",
		staticBarkResolver{"bark.example": {netip.MustParseAddr("93.184.216.34")}},
		pipeBarkDialer("HTTP/1.1 200 OK\r\nContent-Length: 32\r\n\r\n{\"code\":200,\"message\":\"success\"}"),
	)
	require.NoError(t, err)

	s := NewSender(zap.NewNop(), nil, "")
	res, err := s.sendBarkWithPolicy(t.Context(), domain.Device{
		Transport:         domain.DeviceTransportBark,
		TransportEndpoint: "http://bark.example/device-key",
	}, payload.NewPayload().AlertTitle("hi"), p)
	require.NoError(t, err)
	assert.True(t, res.Sent)
}

func TestSendAPNS_NilClientDoesNotUnregister(t *testing.T) {
	t.Parallel()

	s := NewSender(zap.NewNop(), nil, "")

	for _, sandbox := range []bool{false, true} {
		d := domain.Device{Transport: domain.DeviceTransportAPNS, APNSToken: "abc", Sandbox: sandbox}

		res, err := s.Send(t.Context(), d, payload.NewPayload().AlertTitle("hi"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "APNs not configured")
		assert.False(t, res.Sent)
		assert.False(t, res.ShouldUnregister)
	}
}
