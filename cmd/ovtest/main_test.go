package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/cases"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/internal/cli"
	"code.byted.org/data-arch/ovtest/runner"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args   []string
		repeat int
		out    string
		names  []string
	}{
		{nil, 1, "", nil},
		{[]string{"ov-memory"}, 1, "", []string{"ov-memory"}},
		{[]string{"--repeat", "5", "ov-memory"}, 5, "", []string{"ov-memory"}},
		{[]string{"--out", "r.jsonl", "--repeat", "2", "a", "b"}, 2, "r.jsonl", []string{"a", "b"}},
	}
	for _, c := range cases {
		repeat, out, names, err := parseArgs(c.args)
		if err != nil {
			t.Fatalf("parseArgs(%v) unexpected error: %v", c.args, err)
		}
		if repeat != c.repeat || out != c.out || !reflect.DeepEqual(names, c.names) {
			t.Errorf("parseArgs(%v) = (%d,%q,%v) want (%d,%q,%v)",
				c.args, repeat, out, names, c.repeat, c.out, c.names)
		}
	}
}

func TestParseArgsRejectsInvalidValues(t *testing.T) {
	cases := [][]string{
		{"--repeat"},
		{"--repeat", "0"},
		{"--repeat", "-1"},
		{"--repeat", "many"},
		{"--out"},
		{"--bad"},
		{"-bad"},
	}
	for _, args := range cases {
		if _, _, _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%v) should fail", args)
		}
	}
}

func TestRunReturnsUsageCodeForInvalidArgs(t *testing.T) {
	if code := run([]string{"--repeat", "0", "ov-memory"}); code != 2 {
		t.Fatalf("run invalid args exit = %d, want 2", code)
	}
	if code := run([]string{"--unknown"}); code != 2 {
		t.Fatalf("run unknown flag exit = %d, want 2", code)
	}
}

func TestRunPreflightsRecordWrite(t *testing.T) {
	runs := 0
	restore := registerPassingCase(t, "fake-pass", &runs)
	defer restore()

	code, stderr := captureStderr(t, func() int {
		return run([]string{"--out", t.TempDir(), "fake-pass"})
	})
	if code != 1 {
		t.Fatalf("run unwritable --out exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "could not write record") {
		t.Fatalf("run stderr = %q, want record write error", stderr)
	}
	if runs != 0 {
		t.Fatalf("run executed %d case(s) before rejecting unwritable --out", runs)
	}
}

func TestRunAutoPreflightsOpenVikingCases(t *testing.T) {
	runs := 0
	restore := registerPassingCase(t, "ov-auto-preflight", &runs)
	defer restore()

	orig := preflightOpenVikingUser
	preflightCalls := 0
	preflightOpenVikingUser = func() error {
		preflightCalls++
		return nil
	}
	defer func() { preflightOpenVikingUser = orig }()

	if code := run([]string{"ov-auto-preflight"}); code != 0 {
		t.Fatalf("run exit = %d, want 0", code)
	}
	if preflightCalls != 1 {
		t.Fatalf("preflight calls = %d, want 1", preflightCalls)
	}
	if runs != 1 {
		t.Fatalf("case runs = %d, want 1", runs)
	}
}

func TestOpenVikingPreflightMissingConfigIsHardStop(t *testing.T) {
	t.Setenv("OV_TEST_CONF_DIR", t.TempDir())
	err := preflightOpenVikingUser()
	if err == nil || !strings.Contains(err.Error(), "missing OpenViking user config") {
		t.Fatalf("preflight error = %v, want missing-config hard stop", err)
	}
}

func TestRunReportsOpenVikingPreflightFailureBeforeCases(t *testing.T) {
	runs := 0
	restore := registerPassingCase(t, "hermes-openviking-auto-preflight", &runs)
	defer restore()

	orig := preflightOpenVikingUser
	preflightOpenVikingUser = func() error {
		return os.ErrPermission
	}
	defer func() { preflightOpenVikingUser = orig }()

	code, stderr := captureStderr(t, func() int {
		return run([]string{"hermes-openviking-auto-preflight"})
	})
	if code != 1 {
		t.Fatalf("run exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "OpenViking preflight") {
		t.Fatalf("stderr = %q, want preflight failure", stderr)
	}
	if runs != 0 {
		t.Fatalf("case should not run after preflight failure; runs=%d", runs)
	}
}

func TestRunDoesNotRunAPICleanupByDefault(t *testing.T) {
	runs := 0
	restore := registerPassingCase(t, "fake-pass-no-cleanup", &runs)
	defer restore()

	orig := cli.RunVikingCleanup
	cleanupCalls := 0
	cli.RunVikingCleanup = func() cli.Result {
		cleanupCalls++
		return cli.Result{ExitCode: 0}
	}
	defer func() { cli.RunVikingCleanup = orig }()

	if code := run([]string{"fake-pass-no-cleanup"}); code != 0 {
		t.Fatalf("run exit = %d, want 0", code)
	}
	if runs != 1 {
		t.Fatalf("case runs = %d, want 1", runs)
	}
	if cleanupCalls != 0 {
		t.Fatalf("api cleanup calls = %d, want 0 by default", cleanupCalls)
	}
}

func TestRunRunsAPICleanupOnlyWhenExplicit(t *testing.T) {
	runs := 0
	restore := registerPassingCase(t, "fake-pass-api-cleanup", &runs)
	defer restore()

	orig := cli.RunVikingCleanup
	cleanupCalls := 0
	cli.RunVikingCleanup = func() cli.Result {
		cleanupCalls++
		return cli.Result{ExitCode: 0}
	}
	defer func() { cli.RunVikingCleanup = orig }()

	t.Setenv("OV_TEST_CLEANUP_MODE", "api")
	if code := run([]string{"fake-pass-api-cleanup"}); code != 0 {
		t.Fatalf("run exit = %d, want 0", code)
	}
	if cleanupCalls != 1 {
		t.Fatalf("api cleanup calls = %d, want 1 when explicitly enabled", cleanupCalls)
	}
}

func TestRunResetLocalStateReportsRemovedPaths(t *testing.T) {
	orig := resetLocalState
	resetLocalState = func() ([]string, error) {
		return []string{"/tmp/ovtest/hermes/capture-1", "/tmp/ovtest/openviking/workspace"}, nil
	}
	defer func() { resetLocalState = orig }()

	code, stdout := captureStdout(t, func() int {
		return runResetLocalState(nil)
	})
	if code != 0 {
		t.Fatalf("reset-local-state exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "reset-local-state: removed 2 path(s)") ||
		!strings.Contains(stdout, "/tmp/ovtest/hermes/capture-1") ||
		!strings.Contains(stdout, "/tmp/ovtest/openviking/workspace") {
		t.Fatalf("reset-local-state stdout = %q", stdout)
	}
}

func TestRunBootstrapOpenVikingUserReportsWithoutLeakingKey(t *testing.T) {
	orig := bootstrapOpenVikingUser
	bootstrapOpenVikingUser = func() (cli.BootstrapResult, error) {
		return cli.BootstrapResult{
			AccountID:     "loop-ovtest",
			UserID:        "hermes",
			UserKeyLength: 111,
			UpdatedPaths:  []string{"/tmp/ovcli.conf", "/tmp/ovcli.conf.root"},
		}, nil
	}
	defer func() { bootstrapOpenVikingUser = orig }()

	code, stdout := captureStdout(t, func() int {
		return runBootstrapOpenVikingUser(nil)
	})
	if code != 0 {
		t.Fatalf("bootstrap-openviking-user exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "bootstrap-openviking-user: account=loop-ovtest user=hermes") ||
		!strings.Contains(stdout, "updated /tmp/ovcli.conf") ||
		!strings.Contains(stdout, "user_key_length=111") {
		t.Fatalf("bootstrap stdout = %q", stdout)
	}
	if strings.Contains(stdout, "fresh-user-key") {
		t.Fatalf("bootstrap output leaked key: %q", stdout)
	}
}

func TestAppendRecordKeepsRedactionPlaceholderReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	res := runner.Result{
		ID:     "c",
		Status: runner.StatusDetermFail,
		Detail: `raw "user_key":"secret-value"`,
	}
	if err := appendRecord(path, res); err != nil {
		t.Fatalf("appendRecord: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "secret-value") {
		t.Fatalf("record leaked secret: %s", text)
	}
	if !strings.Contains(text, `<redacted>`) {
		t.Fatalf("record should keep redaction placeholder readable: %s", text)
	}
	if strings.Contains(text, `\u003credacted\u003e`) {
		t.Fatalf("record HTML-escaped redaction placeholder: %s", text)
	}
}

func TestParseReportArgs(t *testing.T) {
	only, path, err := parseReportArgs([]string{"runs.jsonl", "--case", "ov-memory"})
	if err != nil {
		t.Fatalf("parseReportArgs unexpected error: %v", err)
	}
	if only != "ov-memory" || path != "runs.jsonl" {
		t.Fatalf("parseReportArgs = (%q,%q), want (ov-memory,runs.jsonl)", only, path)
	}
}

func TestParseReportArgsRejectsInvalidValues(t *testing.T) {
	cases := [][]string{
		nil,
		{"--case"},
		{"--bad", "runs.jsonl"},
		{"a.jsonl", "b.jsonl"},
	}
	for _, args := range cases {
		if _, _, err := parseReportArgs(args); err == nil {
			t.Errorf("parseReportArgs(%v) should fail", args)
		}
	}
}

func TestRunReportReturnsUsageCodeForUnknownFlag(t *testing.T) {
	if code := runReport([]string{"--bad", "runs.jsonl"}); code != 2 {
		t.Fatalf("runReport unknown flag exit = %d, want 2", code)
	}
}

func TestRunReportReportsReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.jsonl")
	code, stderr := captureStderr(t, func() int {
		return runReport([]string{path})
	})
	if code != 1 {
		t.Fatalf("runReport missing file exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "could not read records") || !strings.Contains(stderr, path) {
		t.Fatalf("runReport stderr = %q, want read error with path", stderr)
	}
}

func TestRunReportReturnsFailureForMissingCase(t *testing.T) {
	path := t.TempDir() + "/runs.jsonl"
	if err := os.WriteFile(path, []byte(`{"id":"ov-memory","status":"pass"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runReport([]string{path, "--case", "missing"}); code != 1 {
		t.Fatalf("runReport missing case exit = %d, want 1", code)
	}
	if code := runReport([]string{path, "--case", "ov-memory"}); code != 0 {
		t.Fatalf("runReport matching case exit = %d, want 0", code)
	}
}

func TestRunReportWarnsAboutCorruptRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	body := `{"id":"ov-memory","status":"pass"}` + "\n" +
		"not-json\n" +
		`{"id":` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stderr := captureStderr(t, func() int {
		return runReport([]string{path, "--case", "ov-memory"})
	})
	if code != 0 {
		t.Fatalf("runReport mixed valid/corrupt exit = %d, want 0", code)
	}
	if !strings.Contains(stderr, "warning: ignored 2 corrupt JSONL row(s)") ||
		!strings.Contains(stderr, path) {
		t.Fatalf("runReport stderr = %q, want corrupt row warning with path", stderr)
	}
}

func TestRunReportWarnsAboutInvalidRecordRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	body := "null\n" +
		"{}\n" +
		`{"id":"ov-memory","status":"pass"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stderr := captureStderr(t, func() int {
		return runReport([]string{path, "--case", "ov-memory"})
	})
	if code != 0 {
		t.Fatalf("runReport mixed valid/invalid exit = %d, want 0", code)
	}
	if !strings.Contains(stderr, "warning: ignored 2 invalid JSONL record row(s)") ||
		!strings.Contains(stderr, path) {
		t.Fatalf("runReport stderr = %q, want invalid row warning with path", stderr)
	}
}

func TestRunReportNoValidRecordsSummarizesIgnoredRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	body := "null\nnot-json\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout := captureStdout(t, func() int {
		return runReport([]string{path})
	})
	if code != 1 {
		t.Fatalf("runReport invalid-only records exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "no valid records") ||
		!strings.Contains(stdout, "1 corrupt JSONL row(s), 1 invalid JSONL record row(s) ignored") {
		t.Fatalf("runReport stdout = %q, want ignored row summary", stdout)
	}
}

func captureStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	code := fn()
	os.Stderr = old
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return code, string(out)
}

func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	code := fn()
	os.Stdout = old
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return code, string(out)
}

func registerPassingCase(t *testing.T, id string, runs *int) func() {
	t.Helper()
	orig, existed := cases.All[id]
	cases.All[id] = runner.Case{
		ID: id,
		Build: func(b *dag.Builder) {
			b.Add(dag.Factory{
				Meta:     dag.Meta{Outputs: []string{"verdict"}},
				Terminal: true,
				New: func(string, map[string]any) dag.Op {
					return dag.OpFunc(func(map[string]any) (map[string]any, error) {
						if runs != nil {
							(*runs)++
						}
						return map[string]any{"verdict": map[string]any{"pass": true}}, nil
					})
				},
			}, dag.Spec{Name: "ov_judge"})
		},
	}
	return func() {
		if existed {
			cases.All[id] = orig
		} else {
			delete(cases.All, id)
		}
	}
}
