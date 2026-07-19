package releasegate

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"unicode/utf8"
)

var (
	credentialAssignment = regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|refresh[_-]?token|token|password|secret|signature|sig|x-amz-signature|x-goog-signature|credential)=([^&\s"'\\]+)`)
	authorizationHeader  = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?(?:bearer|basic)\s+)[^\s,"'\\]+`)
	jsonCredential       = regexp.MustCompile(`(?i)("(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|password|secret|authorization)"\s*:\s*")[^"]*(")`)
)

func RedactBytes(raw []byte, secrets []string) []byte {
	out := append([]byte(nil), raw...)
	values := append([]string(nil), secrets...)
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, secret := range values {
		if secret != "" {
			out = bytes.ReplaceAll(out, []byte(secret), []byte("[REDACTED]"))
		}
	}
	out = credentialAssignment.ReplaceAll(out, []byte("$1=[REDACTED]"))
	out = authorizationHeader.ReplaceAll(out, []byte("$1[REDACTED]"))
	out = jsonCredential.ReplaceAll(out, []byte("$1[REDACTED]$2"))
	return out
}

func RedactText(text string, secrets []string) string {
	return string(RedactBytes([]byte(text), secrets))
}

// RedactEvidenceTree scrubs retained textual evidence in place. Non-regular
// files and binary payloads are left untouched; release evidence is expected to
// be JSON, JSONL, or plain-text logs.
func RedactEvidenceTree(root string, secrets []string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read evidence %s: %w", path, err)
		}
		if !utf8.Valid(raw) {
			return nil
		}
		redacted := RedactBytes(raw, secrets)
		if bytes.Equal(raw, redacted) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".redacted-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if err := tmp.Chmod(info.Mode().Perm()); err != nil {
			tmp.Close()
			return err
		}
		if _, err := tmp.Write(redacted); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(tmpName, path)
	})
}
