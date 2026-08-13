package webhook

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSelfHostedWebhookManifestPathInSync guards the contract between the Go
// registration path and the embedded ValidatingWebhookConfiguration manifest:
// if the two drift apart, admission requests 404 and the guardrail silently
// stops working.
func TestSelfHostedWebhookManifestPathInSync(t *testing.T) {
	t.Parallel()

	manifest, err := os.ReadFile(
		"../../provider/kommodity/kommodity-validating-webhook-configuration.yaml")
	require.NoError(t, err, "reading the kommodity webhook manifest")

	require.Contains(t, string(manifest), "path: "+selfHostedClusterValidationPath,
		"manifest must declare the same webhook path the validator registers")
}
