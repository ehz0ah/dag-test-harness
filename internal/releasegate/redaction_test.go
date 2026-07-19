package releasegate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactorCoversExactSecretsHeadersJSONAndSignedURLs(t *testing.T) {
	input := `exact-value Authorization: Bearer abc.def {"api_key":"json-secret"} https://host/upload?X-Amz-Signature=deadbeef&token=query-secret`
	redacted := RedactText(input, []string{"exact-value"})
	for _, forbidden := range []string{"exact-value", "abc.def", "json-secret", "deadbeef", "query-secret"} {
		if strings.Contains(redacted, forbidden) {
			t.Fatalf("redaction leaked %q: %s", forbidden, redacted)
		}
	}
	if strings.Count(redacted, "[REDACTED]") < 5 {
		t.Fatalf("redaction result = %s", redacted)
	}
}

func TestRedactorPreservesJSONEscapesAroundNestedSignedURL(t *testing.T) {
	raw, err := json.Marshal(map[string]string{
		"stdout": `curl "https://host/upload?token=signed-value"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	redacted := RedactBytes(raw, nil)
	if !json.Valid(redacted) {
		t.Fatalf("redaction produced invalid JSON: %s", redacted)
	}
	if strings.Contains(string(redacted), "signed-value") {
		t.Fatalf("redaction leaked signed URL token: %s", redacted)
	}
}

func TestRedactEvidenceTreeScrubsTextAndPreservesBinary(t *testing.T) {
	root := t.TempDir()
	textPath := filepath.Join(root, "server.log")
	binaryPath := filepath.Join(root, "payload.bin")
	if err := os.WriteFile(textPath, []byte("Authorization: Bearer secret-token\nplain-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0xff, 0x00, 0x01}
	if err := os.WriteFile(binaryPath, binary, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RedactEvidenceTree(root, []string{"plain-secret"}); err != nil {
		t.Fatal(err)
	}
	redacted, _ := os.ReadFile(textPath)
	if strings.Contains(string(redacted), "secret-token") || strings.Contains(string(redacted), "plain-secret") {
		t.Fatalf("retained evidence leaked a secret: %s", redacted)
	}
	gotBinary, _ := os.ReadFile(binaryPath)
	if string(gotBinary) != string(binary) {
		t.Fatalf("binary evidence changed: %v", gotBinary)
	}
}
