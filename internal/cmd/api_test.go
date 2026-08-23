package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPICmdFailsClosedBeforeStartupWithoutRegistrationSecret(t *testing.T) {
	t.Setenv("REGISTRATION_SECRET", "")
	t.Setenv("ENV", "production")
	t.Setenv("APOLLO_UNSAFE_ALLOW_MISSING_REGISTRATION_SECRET", "")

	cmd := APICmd(context.Background())
	cmd.SetArgs(nil)
	err := cmd.Execute()
	require.ErrorContains(t, err, "REGISTRATION_SECRET must be set")
}
