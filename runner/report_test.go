package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// report: the capability-absent (every run dies at the SAME node) vs flaky (mixed
// pass/fail) split is the signal that separates a missing capability from a
// non-deterministic wobble.

func rec(id, status, failedNode string, attempts, durations map[string]any) map[string]any {
	return map[string]any{"id": id, "status": status, "failed_node": failedNode,
		"attempts": attempts, "durations": durations}
}

func TestAttribution(t *testing.T) {
	cases := []struct {
		name string
		recs []map[string]any
		want string
	}{
		{"green", []map[string]any{rec("c", "pass", "", nil, nil), rec("c", "pass", "", nil, nil)}, "green"},
		{"capability-absent", []map[string]any{
			rec("c", "deterministic_check_failed", "find", nil, nil),
			rec("c", "deterministic_check_failed", "find", nil, nil)}, "capability-absent"},
		{"flaky", []map[string]any{rec("c", "pass", "", nil, nil),
			rec("c", "semantic_failed", "judge", nil, nil)}, "flaky"},
		{"mixed", []map[string]any{
			rec("c", "deterministic_check_failed", "find", nil, nil),
			rec("c", "deterministic_check_failed", "commit", nil, nil)}, "mixed-failure"},
	}
	for _, c := range cases {
		if got := Attribution(c.recs); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestPctNearestRank(t *testing.T) {
	if _, ok := pct(nil, 50); ok {
		t.Error("empty -> not ok")
	}
	for _, c := range []struct {
		vals []float64
		p    int
		want float64
	}{
		{[]float64{5}, 50, 5},
		{[]float64{1, 2, 3}, 50, 2},
		{[]float64{1, 2, 3, 4}, 100, 4},
		{[]float64{1, 3}, 50, 1}, // round-half-to-even -> index 0
	} {
		if got, _ := pct(c.vals, c.p); got != c.want {
			t.Errorf("pct(%v,%d)=%v want %v", c.vals, c.p, got, c.want)
		}
	}
}

func TestCaseReport(t *testing.T) {
	recs := []map[string]any{
		rec("ov-memory", "pass", "", map[string]any{"find": 1.0}, map[string]any{"find": 0.5}),
		rec("ov-memory", "deterministic_check_failed", "commit", nil, nil),
		rec("ov-memory", "pass", "", map[string]any{"find": 3.0}, map[string]any{"find": 0.9}),
	}
	text := CaseReport("ov-memory", recs)
	for _, want := range []string{"pass-rate 2/3", "flaky", "commit×1", "find=1/3"} {
		if !contains(text, want) {
			t.Errorf("case report missing %q in:\n%s", want, text)
		}
	}
}

func TestCaseReportToleratesNonNumeric(t *testing.T) {
	recs := []map[string]any{
		rec("c", "pass", "", nil, map[string]any{"find": 1.5}),
		rec("c", "pass", "", nil, map[string]any{"find": nil}),
	}
	if text := CaseReport("c", recs); !contains(text, "duration_s p50/max: find=1.5/1.5") {
		t.Errorf("must tolerate None metric rows:\n%s", text)
	}
}

func TestReportFiltersByCase(t *testing.T) {
	recs := []map[string]any{rec("a", "pass", "", nil, nil), rec("b", "pass", "", nil, nil)}
	out := Report(recs, "a")
	if !contains(out, "=== a") || contains(out, "=== b") {
		t.Errorf("filter by case: %s", out)
	}
}

func TestLoadRecordsWithStatsCountsCorruptRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	body := `{"id":"a","status":"pass"}` + "\n" +
		"not-json\n" +
		"\n" +
		`{"id":"b","status":"deterministic_check_failed"}` + "\n" +
		`{"id":` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRecordsWithStats(path)
	if err != nil {
		t.Fatalf("LoadRecordsWithStats: %v", err)
	}
	if len(loaded.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(loaded.Records))
	}
	if loaded.CorruptRows != 2 {
		t.Fatalf("CorruptRows = %d, want 2", loaded.CorruptRows)
	}
	if loaded.InvalidRows != 0 {
		t.Fatalf("InvalidRows = %d, want 0", loaded.InvalidRows)
	}
}

func TestLoadRecordsWithStatsCountsInvalidRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	body := "null\n" +
		"{}\n" +
		`{"id":"missing-status"}` + "\n" +
		`{"status":"missing-id"}` + "\n" +
		`{"id":"a","status":"pass"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRecordsWithStats(path)
	if err != nil {
		t.Fatalf("LoadRecordsWithStats: %v", err)
	}
	if len(loaded.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(loaded.Records))
	}
	if loaded.InvalidRows != 4 {
		t.Fatalf("InvalidRows = %d, want 4", loaded.InvalidRows)
	}
	if loaded.CorruptRows != 0 {
		t.Fatalf("CorruptRows = %d, want 0", loaded.CorruptRows)
	}
}
