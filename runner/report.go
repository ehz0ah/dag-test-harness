package runner

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// report: trend & flake-attribution reporter over the per-run records `--out`
// appends. Per case it prints pass-rate, a status histogram, where failures
// localise, and per-node attempts/latency percentiles. It separates a
// CAPABILITY-ABSENT case (every run dies at the same node) from a FLAKY one (mixed
// pass/fail) — the signal you want running per-release against stochastic agents.

type LoadRecordStats struct {
	Records     []map[string]any
	CorruptRows int
	InvalidRows int
}

// LoadRecords reads runs.jsonl, tolerating blank lines and corrupt rows.
func LoadRecords(path string) ([]map[string]any, error) {
	loaded, err := LoadRecordsWithStats(path)
	if err != nil {
		return nil, err
	}
	return loaded.Records, nil
}

// LoadRecordsWithStats reads runs.jsonl and counts corrupt non-blank rows.
func LoadRecordsWithStats(path string) (LoadRecordStats, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LoadRecordStats{}, err
	}
	loaded := LoadRecordStats{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			if !validRecord(rec) {
				loaded.InvalidRows++
				continue
			}
			loaded.Records = append(loaded.Records, rec)
		} else {
			loaded.CorruptRows++
		}
	}
	return loaded, nil
}

func validRecord(rec map[string]any) bool {
	return rec != nil && asString(rec["id"]) != "" && asString(rec["status"]) != ""
}

// pct returns the p-th percentile (0..100) by nearest-rank (round-half-to-even, to
// match the Python reference), or (0,false) if no numeric values.
func pct(values []float64, p int) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	vals := append([]float64(nil), values...)
	sort.Float64s(vals)
	k := int(math.RoundToEven((float64(p) / 100) * float64(len(vals)-1)))
	if k < 0 {
		k = 0
	}
	if k > len(vals)-1 {
		k = len(vals) - 1
	}
	return vals[k], true
}

func numbers(values []any) []float64 {
	var out []float64
	for _, v := range values {
		switch x := v.(type) {
		case float64:
			out = append(out, x)
		case int:
			out = append(out, float64(x))
		}
	}
	return out
}

func numfmt(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// p50max formats "<p50>/<max>" over the numeric values, or "" if none are numeric.
func p50max(values []any) string {
	nums := numbers(values)
	if len(nums) == 0 {
		return ""
	}
	p50, _ := pct(nums, 50)
	mx := nums[0]
	for _, v := range nums {
		if v > mx {
			mx = v
		}
	}
	return numfmt(p50) + "/" + numfmt(mx)
}

func perNode(records []map[string]any, key string) map[string][]any {
	acc := map[string][]any{}
	for _, r := range records {
		m, _ := r[key].(map[string]any)
		for node, v := range m {
			acc[node] = append(acc[node], v)
		}
	}
	return acc
}

// Attribution classifies a case's failure pattern.
func Attribution(records []map[string]any) string {
	passes := 0
	nodes := map[string]bool{}
	fails := 0
	for _, r := range records {
		if asString(r["status"]) == "pass" {
			passes++
			continue
		}
		fails++
		fn := asString(r["failed_node"])
		if fn == "" {
			fn = "-"
		}
		nodes[fn] = true
	}
	if fails == 0 {
		return "green"
	}
	if passes > 0 {
		return "flaky"
	}
	if len(nodes) == 1 {
		return "capability-absent"
	}
	return "mixed-failure"
}

// CaseReport renders the per-case trend block.
func CaseReport(cid string, records []map[string]any) string {
	n := len(records)
	statusCount := map[string]int{}
	passes := 0
	var fails []map[string]any
	for _, r := range records {
		s := asString(r["status"])
		if s == "" {
			s = "?"
		}
		statusCount[s]++
		if s == "pass" {
			passes++
		} else {
			fails = append(fails, r)
		}
	}

	lines := []string{
		fmt.Sprintf("\n=== %s — pass-rate %d/%d  [%s] ===", cid, passes, n, Attribution(records)),
		"  status: " + joinCounter(statusCount, "="),
	}

	if len(fails) > 0 {
		fn := map[string]int{}
		for _, r := range fails {
			node := asString(r["failed_node"])
			if node == "" {
				node = "-"
			}
			fn[node]++
		}
		lines = append(lines, "  failures: "+joinCounter(fn, "×"))
	}

	for _, lk := range []struct{ label, key string }{{"attempts", "attempts"}, {"duration_s", "durations"}} {
		var cells []string
		nodes := perNode(records, lk.key)
		names := make([]string, 0, len(nodes))
		for name := range nodes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if c := p50max(nodes[name]); c != "" {
				cells = append(cells, name+"="+c)
			}
		}
		if len(cells) > 0 {
			lines = append(lines, fmt.Sprintf("  %s p50/max: %s", lk.label, strings.Join(cells, ", ")))
		}
	}

	soft := map[string]int{}
	for _, r := range records {
		sfs, _ := r["soft_failures"].([]any)
		for _, sf := range sfs {
			if m, ok := sf.(map[string]any); ok {
				soft[asString(m["node"])]++
			}
		}
	}
	if len(soft) > 0 {
		lines = append(lines, "  soft gate failures: "+joinCounter(soft, "×"))
	}
	return strings.Join(lines, "\n")
}

// Report renders all cases (or one), in sorted case order.
func Report(records []map[string]any, only string) string {
	byCase := map[string][]map[string]any{}
	for _, r := range records {
		byCase[asString(r["id"])] = append(byCase[asString(r["id"])], r)
	}
	ids := make([]string, 0, len(byCase))
	for id := range byCase {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []string
	for _, id := range ids {
		if only == "" || id == only {
			out = append(out, CaseReport(id, byCase[id]))
		}
	}
	if len(out) == 0 {
		return "no matching records"
	}
	return strings.Join(out, "\n")
}

// joinCounter renders a count map most-common first (ties broken by name), each
// entry as "<key><sep><count>".
func joinCounter(m map[string]int, sep string) string {
	type kv struct {
		k string
		c int
	}
	items := make([]kv, 0, len(m))
	for k, c := range m {
		items = append(items, kv{k, c})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].c != items[j].c {
			return items[i].c > items[j].c
		}
		return items[i].k < items[j].k
	})
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s%s%d", it.k, sep, it.c))
	}
	return strings.Join(parts, ", ")
}
