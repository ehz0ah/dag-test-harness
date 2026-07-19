package support

import (
	"os"
	"strings"
)

const DefaultRemoteResourceURL = "https://raw.githubusercontent.com/ehz0ah/Celeste/5b8ab7f10d10/README.md"

var defaultRemoteResourceExpect = []string{"celeste fpga", "basys3", "verilog"}

// RemoteResourceURL resolves a harness-specific override before the common
// release-suite override. The default is immutable because it is content pinned.
func RemoteResourceURL(specificEnv string) string {
	if specificEnv != "" {
		if value := strings.TrimSpace(os.Getenv(specificEnv)); value != "" {
			return value
		}
	}
	if value := strings.TrimSpace(os.Getenv("OV_TEST_REMOTE_RESOURCE_URL")); value != "" {
		return value
	}
	return DefaultRemoteResourceURL
}

// RemoteResourceExpect returns stable semantic markers for the pinned fixture.
// Individual tokens intentionally tolerate punctuation and descriptive words
// introduced by OpenViking extraction while still identifying the resource.
func RemoteResourceExpect(specificEnv string) []string {
	raw := ""
	if specificEnv != "" {
		raw = strings.TrimSpace(os.Getenv(specificEnv))
	}
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("OV_TEST_REMOTE_RESOURCE_EXPECT"))
	}
	if raw == "" {
		return append([]string(nil), defaultRemoteResourceExpect...)
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if token := strings.ToLower(strings.TrimSpace(part)); token != "" {
			out = append(out, token)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), defaultRemoteResourceExpect...)
	}
	return out
}
