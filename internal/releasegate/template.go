package releasegate

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed local-openviking.json
var defaultLocalOpenVikingConfig []byte

// WriteDefaultLocalOpenVikingTemplate materializes the bundled, secret-free
// local service template. Provider values remain environment references.
func WriteDefaultLocalOpenVikingTemplate(path string) error {
	if path == "" {
		return fmt.Errorf("local OpenViking template path is required")
	}
	var decoded map[string]any
	if err := json.Unmarshal(defaultLocalOpenVikingConfig, &decoded); err != nil {
		return fmt.Errorf("bundled local OpenViking template: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, defaultLocalOpenVikingConfig, 0o600)
}
