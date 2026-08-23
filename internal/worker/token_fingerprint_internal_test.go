package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTokenFingerprint_IsStableAndDoesNotExposeToken(t *testing.T) {
	t.Parallel()

	const token = "apns-and-bark-bearer-token"
	got := tokenFingerprint(token)

	assert.Equal(t, "sha256:1cbdb8d42c643e96", got)
	assert.Equal(t, got, tokenFingerprint(token))
	assert.NotContains(t, got, token)
	assert.NotEqual(t, got, tokenFingerprint("different-token"))
	assert.Empty(t, tokenFingerprint(""))
}
