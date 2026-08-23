package worker

import (
	"crypto/sha256"
	"encoding/hex"
)

const tokenFingerprintHexLength = 16

// tokenFingerprint is suitable for structured logs: it retains stable
// correlation across worker events without retaining an APNs, Bark, or
// ActivityKit bearer token itself.
func tokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])[:tokenFingerprintHexLength]
}
