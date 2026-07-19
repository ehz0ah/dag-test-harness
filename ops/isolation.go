package ops

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SecretStateDir returns a stable, run-scoped directory for generated files
// that may contain credentials. An empty result means the caller is running in
// the legacy/manual mode where no separate secret root was configured.
func SecretStateDir(namespace, stateDir string) string {
	root := strings.TrimSpace(os.Getenv("OV_TEST_SECRET_STATE_DIR"))
	if root == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return filepath.Join(root, safePathSegment(namespace), fmt.Sprintf("%x", digest[:8]))
}

// QueueScopeKeyFile returns the environment-wide queue-scope key when the
// release runner provides one. Legacy/manual runs fall back to the isolated
// state secret directory, never the replaceable plugin installation.
func QueueScopeKeyFile(namespace, stateDir string) string {
	if explicit := strings.TrimSpace(os.Getenv("OPENVIKING_QUEUE_SCOPE_KEY_FILE")); explicit != "" {
		return explicit
	}
	if secret := SecretStateDir(namespace, stateDir); secret != "" {
		return filepath.Join(secret, "queue-scope.key")
	}
	return filepath.Join(stateDir, "secrets", "queue-scope.key")
}

func safePathSegment(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteByte('-')
		}
	}
	if clean := strings.Trim(out.String(), "-"); clean != "" {
		return clean
	}
	return "state"
}
