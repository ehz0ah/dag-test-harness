package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops"
)

type waitForContextOp struct{}

func (waitForContextOp) Process(map[string]any) (map[string]any, error) {
	return nil, errors.New("context execution required")
}

func (waitForContextOp) ProcessContext(ctx context.Context, _ map[string]any) (map[string]any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// runner: drive RunCase through each of the 5 status branches with tiny
// in-process DAGs (no live services, no LLM), and pin the terminal-judge
// structural invariant and soft-failure surfacing.

var (
	fakePass     = fakeStaticFactory(map[string]any{"ok": true}, nil)
	fakeText     = fakeFactory([]string{"text"}, map[string]any{"text": "PX-123 cobalt teal"}, nil)
	fakeGateFail = dag.Factory{
		Meta: dag.Meta{Inputs: []string{"after"}, Outputs: []string{"ok"}},
		New: func(name string, _ map[string]any) dag.Op {
			return dag.OpFunc(func(map[string]any) (map[string]any, error) {
				return nil, &ops.GateFail{Node: name, Detail: "hard gate tripped"}
			})
		}}
	fakeBoom = fakeStaticFactory(nil, errors.New("kaboom"))
	fakeSoft = dag.Factory{
		Meta: dag.Meta{Inputs: []string{"after"}, Outputs: []string{"ok"}},
		New: func(name string, _ map[string]any) dag.Op {
			return dag.OpFunc(func(map[string]any) (map[string]any, error) {
				return map[string]any{"ok": nil, "gate_failed": map[string]any{"node": name, "detail": "wobbled"}}, nil
			})
		}}
	fakeJudge = dag.Factory{
		Meta:     dag.Meta{Inputs: []string{"after", "memories", "entries", "created", "reply", "transcript", "search_memories"}, Outputs: []string{"verdict"}},
		Terminal: true,
		New: func(_ string, oc map[string]any) dag.Op {
			return dag.OpFunc(func(map[string]any) (map[string]any, error) {
				switch asString(oc["verdict_mode"]) {
				case "judge_error":
					return nil, &ops.JudgeError{Detail: "model 500"}
				case "no_verdict":
					return map[string]any{"verdict": nil}, nil
				case "fail":
					return map[string]any{"verdict": map[string]any{"pass": false, "explanation": "no"}}, nil
				default:
					return map[string]any{"verdict": map[string]any{"pass": true, "explanation": "ok"}}, nil
				}
			})
		}}
)

func fakeStaticFactory(out map[string]any, err error) dag.Factory {
	return fakeFactory([]string{"ok"}, out, err)
}

func fakeFactory(outputs []string, out map[string]any, err error) dag.Factory {
	return dag.Factory{
		Meta: dag.Meta{Inputs: []string{"after"}, Outputs: outputs},
		New: func(string, map[string]any) dag.Op {
			return dag.OpFunc(func(map[string]any) (map[string]any, error) { return out, err })
		},
	}
}

func runFake(build func(b *dag.Builder)) Result {
	return RunCase(Case{ID: "t", Build: build})
}

func judgeCase(mode string) func(b *dag.Builder) {
	return func(b *dag.Builder) {
		step := b.Add(fakePass, dag.Spec{Name: "a"})
		b.Add(fakeJudge, dag.Spec{Name: "judge", In: dag.In{"after": step},
			Config: dag.Cfg{"verdict_mode": mode}})
	}
}

func TestStatusTaxonomy(t *testing.T) {
	cases := []struct {
		mode       string
		wantStatus string
		wantPass   any
	}{
		{"pass", StatusPass, true},
		{"fail", StatusSemanticFail, false},
	}
	for _, c := range cases {
		res := runFake(judgeCase(c.mode))
		if res.Status != c.wantStatus {
			t.Errorf("mode %q: status=%s want %s", c.mode, res.Status, c.wantStatus)
		}
		if res.Verdict["pass"] != c.wantPass {
			t.Errorf("mode %q: verdict=%v", c.mode, res.Verdict)
		}
	}
}

func TestRunCaseAcceptsTerminalTextCheckVerdict(t *testing.T) {
	res := runFake(func(b *dag.Builder) {
		text := b.Add(fakeText, dag.Spec{Name: "text"})
		b.Add(ops.TextCheck, dag.Spec{
			Name: "reply_check",
			In:   dag.In{"text": text.Out("text")},
			Config: dag.Cfg{
				"expect": []string{"PX-123", "cobalt teal"},
			},
		})
	})
	if res.Status != StatusPass {
		t.Fatalf("terminal text check should classify as pass: %+v", res)
	}
	if res.Verdict["pass"] != true || !contains(asString(res.Verdict["explanation"]), "text check passed") {
		t.Fatalf("text check verdict = %v", res.Verdict)
	}
}

func TestTerminalTextCheckFailureInObserveModeIsDeterministic(t *testing.T) {
	t.Setenv("OVTEST_GATE_MODE", "observe")
	res := runFake(func(b *dag.Builder) {
		text := b.Add(fakeText, dag.Spec{Name: "text"})
		b.Add(ops.TextCheck, dag.Spec{
			Name:   "reply_check",
			In:     dag.In{"text": text.Out("text")},
			Config: dag.Cfg{"expect": []string{"not present"}},
		})
	})
	if res.Status != StatusDetermFail || res.FailedNode != "reply_check" {
		t.Fatalf("terminal text check observe failure should stay deterministic: %+v", res)
	}
}

func TestStatusDeterministicCheckFailed(t *testing.T) {
	res := runFake(func(b *dag.Builder) {
		step := b.Add(fakeGateFail, dag.Spec{Name: "a"})
		b.Add(fakeJudge, dag.Spec{Name: "judge", In: dag.In{"after": step}})
	})
	if res.Status != StatusDetermFail || res.FailedNode != "a" || !contains(res.Detail, "hard gate tripped") {
		t.Fatalf("deterministic_check_failed: %+v", res)
	}
}

func TestStatusJudgeFailed(t *testing.T) {
	if res := runFake(judgeCase("judge_error")); res.Status != StatusJudgeFail || !contains(res.Detail, "model 500") {
		t.Errorf("judge_error -> judge_failed: %+v", res)
	}
	if res := runFake(judgeCase("no_verdict")); res.Status != StatusJudgeFail || !contains(res.Detail, "no verdict") {
		t.Errorf("no_verdict -> judge_failed: %+v", res)
	}
}

func TestStatusExecutionFailed(t *testing.T) {
	res := runFake(func(b *dag.Builder) {
		step := b.Add(fakeBoom, dag.Spec{Name: "boom"})
		b.Add(fakeJudge, dag.Spec{Name: "judge", In: dag.In{"after": step}})
	})
	if res.Status != StatusExecutionFail || !contains(res.Detail, "kaboom") {
		t.Fatalf("execution_failed: %+v", res)
	}
}

func TestBuildPanicIsExecutionFailed(t *testing.T) {
	res := RunCase(Case{ID: "bad-build", Build: func(*dag.Builder) { panic("bad wiring") }})
	if res.Status != StatusExecutionFail || !contains(res.Detail, "build panic: bad wiring") {
		t.Fatalf("build panic must be classified as execution_failed: %+v", res)
	}
}

func TestJudgeMustBeTerminal(t *testing.T) {
	res := runFake(func(b *dag.Builder) {
		step := b.Add(fakePass, dag.Spec{Name: "a"})
		judge := b.Add(fakeJudge, dag.Spec{Name: "judge", In: dag.In{"after": step}})
		b.Add(fakePass, dag.Spec{Name: "b", In: dag.In{"after": judge}}) // illegal successor
	})
	if res.Status != StatusExecutionFail || !contains(res.Detail, "must be terminal") {
		t.Fatalf("non-terminal judge must be rejected: %+v", res)
	}
}

func TestRunCaseRequiresExactlyOneTerminalJudge(t *testing.T) {
	res := runFake(func(b *dag.Builder) {
		b.Add(fakePass, dag.Spec{Name: "a"})
	})
	if res.Status != StatusExecutionFail || !contains(res.Detail, "exactly one terminal gate") {
		t.Fatalf("missing terminal judge must be rejected structurally: %+v", res)
	}

	res = runFake(func(b *dag.Builder) {
		step := b.Add(fakePass, dag.Spec{Name: "a"})
		b.Add(fakeJudge, dag.Spec{Name: "judge_1", In: dag.In{"after": step}})
		b.Add(fakeJudge, dag.Spec{Name: "judge_2", In: dag.In{"after": step}})
	})
	if res.Status != StatusExecutionFail || !contains(res.Detail, "exactly one terminal gate") {
		t.Fatalf("multiple terminal judges must be rejected structurally: %+v", res)
	}
}

func TestRunCaseDoesNotCleanupPerCase(t *testing.T) {
	res := RunCase(Case{ID: "t", Build: func(b *dag.Builder) {
		step := b.Add(fakeGateFail, dag.Spec{Name: "a"})
		b.Add(fakeJudge, dag.Spec{Name: "judge", In: dag.In{"after": step}})
	}})
	if _, ok := res.Trace["cleanup"]; ok {
		t.Fatalf("per-case trace must not include cleanup: %+v", res.Trace["cleanup"])
	}
	if res.Status != StatusDetermFail || res.FailedNode != "a" {
		t.Fatalf("primary failure classification changed: %+v", res)
	}
}

func TestRunCaseContextEnforcesConfiguredCaseTimeout(t *testing.T) {
	t.Setenv("OV_TEST_CASE_TIMEOUT", "1")
	wait := dag.Factory{Meta: dag.Meta{Outputs: []string{"ok"}}, New: func(string, map[string]any) dag.Op {
		return waitForContextOp{}
	}}
	start := time.Now()
	res := RunCaseContext(context.Background(), Case{ID: "timeout", Build: func(b *dag.Builder) {
		step := b.Add(wait, dag.Spec{Name: "wait"})
		b.Add(fakeJudge, dag.Spec{Name: "judge", In: dag.In{"after": step}})
	}})
	if res.Status != StatusTimedOut {
		t.Fatalf("timeout status = %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("case timeout took %v", elapsed)
	}
}

func TestSoftFailuresSurfaced(t *testing.T) {
	res := runFake(func(b *dag.Builder) {
		step := b.Add(fakeSoft, dag.Spec{Name: "a"})
		b.Add(fakeJudge, dag.Spec{Name: "judge", In: dag.In{"after": step}})
	})
	if res.Status != "soft_gate_failed" {
		t.Fatalf("soft failure status = %s, want soft_gate_failed", res.Status)
	}
	if len(res.SoftFailures) != 1 || res.SoftFailures[0]["detail"] != "wobbled" {
		t.Fatalf("soft failure must surface: %v", res.SoftFailures)
	}
	if ResultRecord(res)["soft_failures"].([]map[string]any)[0]["node"] != "a" {
		t.Error("soft failures must land in the artifact record")
	}
}

func TestResultRecordIsJSONSafe(t *testing.T) {
	res := Result{ID: "c", Status: StatusPass, Verdict: map[string]any{"pass": true},
		Trace: dag.Trace{
			"find": {Output: map[string]any{"attempts": 3, "duration_s": 1.2}},
			"ls":   {Output: map[string]any{"entries": []any{}}},
		}}
	rec := ResultRecord(res)
	if rec["attempts"].(map[string]any)["find"] != 3 || rec["durations"].(map[string]any)["find"] != 1.2 {
		t.Errorf("metrics: %v %v", rec["attempts"], rec["durations"])
	}
	if _, ok := rec["trace"]; ok {
		t.Error("record must not embed the full trace")
	}
	if _, err := json.Marshal(rec); err != nil {
		t.Errorf("record must be JSON-safe: %v", err)
	}
}

func TestResultRecordRedactsKeyMaterial(t *testing.T) {
	res := Result{
		ID:     "c",
		Status: StatusDetermFail,
		Detail: `raw "user_key":"detail-secret"`,
		Verdict: map[string]any{
			"pass":        false,
			"explanation": `saw "api_key":"verdict-secret" for account acct`,
		},
		SoftFailures: []map[string]any{{
			"node":   "soft",
			"detail": `openclaw setup --api-key argv-secret failed with OPENVIKING_API_KEY=soft-secret`,
		}},
	}
	rec := ResultRecord(res)
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("record must be JSON-safe: %v", err)
	}
	text := string(body)
	for _, secret := range []string{"detail-secret", "verdict-secret", "soft-secret", "argv-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("ResultRecord leaked %q in %s", secret, text)
		}
	}
	if !strings.Contains(asString(rec["detail"]), `"user_key":"<redacted>"`) {
		t.Fatalf("detail not redacted: %v", rec["detail"])
	}
	verdict, _ := rec["verdict"].(map[string]any)
	if !strings.Contains(asString(verdict["explanation"]), `"api_key":"<redacted>"`) ||
		!strings.Contains(asString(verdict["explanation"]), "acct") {
		t.Fatalf("verdict explanation not redacted with context: %v", verdict["explanation"])
	}
	soft := rec["soft_failures"].([]map[string]any)
	if !strings.Contains(asString(soft[0]["detail"]), `--api-key <redacted>`) ||
		!strings.Contains(asString(soft[0]["detail"]), `OPENVIKING_API_KEY=<redacted>`) {
		t.Fatalf("soft failure not redacted: %v", soft[0]["detail"])
	}
	if !strings.Contains(res.Detail, "detail-secret") ||
		!strings.Contains(asString(res.Verdict["explanation"]), "verdict-secret") ||
		!strings.Contains(asString(res.SoftFailures[0]["detail"]), "soft-secret") {
		t.Fatalf("ResultRecord should not mutate the source result: %+v", res)
	}
}

func TestPrintResultRedactsUserKeysInFailureEvidence(t *testing.T) {
	res := Result{
		ID:         "c",
		Status:     StatusDetermFail,
		FailedNode: "create",
		Detail:     `raw "user_key":"detail-secret"`,
		Trace: dag.Trace{
			"create": {Output: map[string]any{
				"exit_code": 0,
				"stdout":    `{"ok":true,"result":{"account_id":"acct","user_key":"stdout-secret"}}`,
			}},
			"chat": {Err: `OPENVIKING_API_KEY=env-secret failed`},
		},
		SoftFailures: []map[string]any{{"node": "soft", "detail": `"api_key":"soft-secret"`}},
	}
	out := captureStdout(t, func() {
		PrintResult(res)
	})
	for _, secret := range []string{"detail-secret", "stdout-secret", "env-secret", "soft-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("PrintResult leaked %q in:\n%s", secret, out)
		}
	}
	for _, want := range []string{`"user_key":"<redacted>"`, `OPENVIKING_API_KEY=<redacted>`, `account_id":"acct`} {
		if !strings.Contains(out, want) {
			t.Fatalf("PrintResult missing %q in:\n%s", want, out)
		}
	}
}

func TestCleanupClaimsAcceptOnlyTypedSuccessfulNodeOutput(t *testing.T) {
	trace := dag.Trace{
		"created": {Output: map[string]any{ops.CleanupClaimsOutput: []ops.CleanupClaim{
			{URI: "viking://user/memories/mem-1", Kind: "memory", Source: "spoofed", Proof: "verified relevant result"},
		}}},
		"plain_strings": {Output: map[string]any{
			ops.CleanupClaimsOutput: []any{map[string]any{"uri": "viking://user/memories/forged"}},
			"text":                  "viking://user/memories/also-forged",
		}},
		"failed": {Err: "boom", Output: map[string]any{ops.CleanupClaimsOutput: []ops.CleanupClaim{
			{URI: "viking://user/memories/failed", Kind: "memory", Proof: "not successful"},
		}}},
	}
	got := cleanupClaims(trace)
	if len(got) != 1 || got[0].URI != "viking://user/memories/mem-1" || got[0].Source != "created" {
		t.Fatalf("cleanupClaims = %+v", got)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	os.Stdout = old
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return string(out)
}
