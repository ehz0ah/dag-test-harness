package support

import "strings"

// MemoryFactExpect returns stable semantic markers for LLM-extracted memory.
// Extraction may add punctuation or inflect a word, so requiring a complete
// human phrase creates false negatives. Each distinct source token remains a
// deterministic requirement.
func MemoryFactExpect(values ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		for _, token := range strings.Fields(strings.ToLower(value)) {
			token = strings.Trim(token, " \t\r\n.,;:!?()[]{}\"'")
			if token != "" && !seen[token] {
				seen[token] = true
				out = append(out, token)
			}
		}
	}
	return out
}
