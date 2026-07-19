package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"code.byted.org/data-arch/ovtest/dag"
)

// judgeOp: the per-case terminal judge — the ONLY LLM. Its input edges ARE the
// evidence it scores (visible in the DAG); config carries the goal + reference. It
// renders exactly the non-nil wired evidence (in EVIDENCE order) and returns the
// verdict as data — raising only on a model/parse failure (JudgeError), never on a
// FAIL verdict. GATE_CRITICAL: a config error here must fail loud (not soften).

func judgeOp() dag.Factory {
	meta := dag.Meta{
		Inputs: []string{"memories", "entries", "created", "reply",
			"transcript", "search_memories", "after"},
		Outputs: []string{"verdict"},
	}
	return factory(meta, true, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			reference, err := b.needStr("reference")
			if err != nil {
				return nil, err
			}
			goal := asString(b.oc["goal"])

			// (input name, renderer) — order = order shown to the judge.
			evidence := []struct {
				key    string
				render func(any) string
			}{
				{"created", func(v any) string { return fmt.Sprintf("configured user present = %v", v != nil) }},
				{"after", renderSteps},
				{"entries", renderJSON("ls entries", 600)},
				{"transcript", renderJSON("openviking session transcript", 1500)},
				{"memories", func(v any) string { return renderMemories(v, "find memories") }},
				{"search_memories", func(v any) string { return renderMemories(v, "search memories") }},
				{"reply", renderJSON("openclaw recall reply", 1500)},
			}
			lines := []string{"EVIDENCE (explicitly wired into the judge):"}
			for _, e := range evidence {
				if v := in[e.key]; v != nil {
					lines = append(lines, "- "+e.render(v))
				}
			}
			verdict, err := arkVerdict(goal, reference, strings.Join(lines, "\n"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"verdict": verdict}, nil
		}
	})
}

// ── evidence renderers ──────────────────────────────────────────────────────--

func renderSteps(v any) string {
	oks, ok := v.([]any)
	if !ok {
		oks = []any{v}
	}
	n := 0
	for _, x := range oks {
		if boolish(x) {
			n++
		}
	}
	return fmt.Sprintf("pipeline steps ok before retrieval: %d/%d", n, len(oks))
}

func renderMemories(v any, label string) string {
	mems := memoriesBySource(asMemList(v))
	out := make([]map[string]any, 0, len(mems))
	for _, m := range mems {
		item := map[string]any{
			"uri":      m["uri"],
			"abstract": truncate(asString(m["abstract"]), 1500),
			"score":    m["score"],
		}
		if source := asString(m["source_node"]); source != "" {
			item["source_node"] = source
		}
		out = append(out, item)
	}
	return label + ": " + truncate(jsonDump(out), 6000)
}

// memoriesBySource keeps the server's relevance order within each retrieval
// while ensuring one large result cannot hide every later evidence source from
// the judge's bounded prompt.
func memoriesBySource(mems []map[string]any) []map[string]any {
	if len(mems) < 2 {
		return mems
	}
	order := make([]string, 0)
	buckets := make(map[string][]map[string]any)
	for _, mem := range mems {
		source := asString(mem["source_node"])
		if _, ok := buckets[source]; !ok {
			order = append(order, source)
		}
		buckets[source] = append(buckets[source], mem)
	}
	if len(order) < 2 {
		return mems
	}

	out := make([]map[string]any, 0, len(mems))
	for index := 0; len(out) < len(mems); index++ {
		for _, source := range order {
			if index < len(buckets[source]) {
				out = append(out, buckets[source][index])
			}
		}
	}
	return out
}

func renderJSON(label string, limit int) func(any) string {
	return func(v any) string { return label + ": " + truncate(jsonDump(v), limit) }
}

func jsonDump(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

func boolish(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	}
	return true
}
