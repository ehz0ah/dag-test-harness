// Package support contains small release-case helpers shared across harnesses.
package support

import (
	"os"
	"strings"
	"unicode"
)

// ResourceRoot returns a run-owned resource prefix. OV_TEST_RUN_ID is supplied
// by the release orchestrator; fallback must already be unique for direct runs.
func ResourceRoot(harness, fallback string) string {
	runID := strings.TrimSpace(os.Getenv("OV_TEST_RUN_ID"))
	if runID == "" {
		runID = fallback
	}
	return "viking://resources/ovtest-runs/" + segment(runID) + "/" + segment(harness)
}

func segment(value string) string {
	var out strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			lastDash = false
		} else if !lastDash && out.Len() > 0 {
			out.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(out.String(), "-")
	if result == "" {
		return "run"
	}
	return result
}
