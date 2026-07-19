package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/internal/cli"
	"code.byted.org/data-arch/ovtest/ops"
)

// runner: build the per-case DAG (including the terminal ov_judge node), execute
// it capturing the full evidence trace, and classify the outcome. The judge runs
// as a node, so its verdict is read from the trace — not invoked here.

// Case is one e2e test: an id, the goal + reference (for the registry/report), and
// a build func that wires the DAG.
type Case struct {
	ID        string
	Goal      string
	Reference string
	Build     func(b *dag.Builder)
}

// Result is the outcome of one run: the status, where it failed, the judge
// verdict (if it ran), any soft-gate failures, and the full evidence trace.
type Result struct {
	SchemaVersion int
	RunID         string
	StartedAt     time.Time
	FinishedAt    time.Time
	ID            string
	Status        string
	FailedNode    string
	Detail        string
	Verdict       map[string]any
	SoftFailures  []map[string]any
	Trace         dag.Trace
	CleanupClaims []ops.CleanupClaim
	ArtifactPath  string
	Provenance    map[string]any
	CleanupStatus string
	CleanupDetail string
}

// Status values.
const (
	StatusPass          = "pass"
	StatusSoftFail      = "soft_gate_failed"
	StatusSemanticFail  = "semantic_failed"
	StatusDetermFail    = "deterministic_check_failed"
	StatusJudgeFail     = "judge_failed"
	StatusExecutionFail = "execution_failed"
	StatusTimedOut      = "timed_out"
	StatusPreflightFail = "preflight_failed"
	StatusCleanupFail   = "cleanup_failed"
)

// RunCase builds, validates, executes, and classifies one case.
func RunCase(c Case) Result {
	return RunCaseContext(context.Background(), c)
}

// RunCaseContext is the cancellation-aware runner entrypoint used by the CLI.
func RunCaseContext(ctx context.Context, c Case) (res Result) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, caseTimeout())
	defer cancel()
	defer ops.CloseFixtureServers()

	res = Result{SchemaVersion: 2, RunID: os.Getenv("OV_TEST_RUN_ID"), StartedAt: time.Now().UTC(), ID: c.ID, Status: StatusPass}
	defer func() { res.FinishedAt = time.Now().UTC() }()

	workflow, err := buildWorkflow(c)
	if err != nil {
		res.Status, res.Detail = StatusExecutionFail, err.Error()
		return res
	}
	ex, err := dag.NewExecutor(workflow)
	if err != nil {
		res.Status, res.Detail = StatusExecutionFail, err.Error()
		return res
	}
	// Structural invariant: each case has exactly one terminal gate.
	if err := checkTerminalJudge(ex, workflow); err != nil {
		res.Status, res.Detail = StatusExecutionFail, err.Error()
		return res
	}

	trace, runErr := ex.Run(ctx)
	res.Trace = trace
	res.CleanupClaims = cleanupClaims(trace)

	if runErr != nil {
		var je *ops.JudgeError
		var gf *ops.GateFail
		switch {
		case errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			res.Status, res.Detail = StatusTimedOut, context.DeadlineExceeded.Error()
		case errors.As(runErr, &je):
			res.Status, res.FailedNode, res.Detail = StatusJudgeFail, "judge", je.Detail
		case errors.As(runErr, &gf):
			res.Status, res.FailedNode, res.Detail = StatusDetermFail, gf.Node, gf.Detail
		default:
			res.Status, res.Detail = StatusExecutionFail, runErr.Error()
		}
	} else {
		verdict := verdictFromTrace(workflow, trace)
		if verdict == nil {
			res.Status, res.FailedNode, res.Detail = StatusJudgeFail, "judge", "no verdict produced"
		} else {
			res.Verdict = verdict
			if boolish(verdict["pass"]) {
				res.Status = StatusPass
			} else {
				res.Status = StatusSemanticFail
			}
		}
	}

	res.SoftFailures = softFailures(workflow, trace)
	if res.Status == StatusPass && len(res.SoftFailures) > 0 {
		res.Status = StatusSoftFail
	}
	return res
}

func cleanupClaims(trace dag.Trace) []ops.CleanupClaim {
	var claims []ops.CleanupClaim
	seen := map[string]bool{}
	for _, node := range sortedTraceNodes(trace) {
		io := trace[node]
		if io.Err != "" || io.Skipped || io.Output == nil {
			continue
		}
		items, ok := io.Output[ops.CleanupClaimsOutput].([]ops.CleanupClaim)
		if !ok {
			continue
		}
		for _, claim := range items {
			if claim.URI == "" || claim.Kind == "" || claim.Proof == "" {
				continue
			}
			claim.Source = node
			key := claim.Kind + "\x00" + claim.URI
			if seen[key] {
				continue
			}
			seen[key] = true
			claims = append(claims, claim)
		}
	}
	return claims
}

func caseTimeout() time.Duration {
	secs := envIntAny([]string{"OV_TEST_CASE_TIMEOUT", "OVTEST_CASE_TIMEOUT"}, 1800)
	if secs <= 0 {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(secs) * time.Second
}

func envIntAny(keys []string, def int) int {
	for _, key := range keys {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err == nil {
			return n
		}
	}
	return def
}

func CleanupViking() (dag.NodeIO, string) {
	r := cli.RunVikingCleanup()
	out := cli.Fields(r)
	out["ok"] = r.ExitCode == 0
	if r.ExitCode == 0 {
		return dag.NodeIO{Input: map[string]any{}, Output: out}, ""
	}
	return dag.NodeIO{Input: map[string]any{}, Output: out}, cli.ExitDetail(r)
}

func buildWorkflow(c Case) (workflow *dag.Dag, err error) {
	b := dag.New()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("build panic: %v", r)
		}
	}()
	if c.Build == nil {
		return nil, fmt.Errorf("build: nil case build")
	}
	c.Build(b)
	workflow, err = b.Build()
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}
	return workflow, nil
}

func checkTerminalJudge(ex *dag.Executor, workflow *dag.Dag) error {
	succ := ex.Successors()
	judges := 0
	for _, name := range workflow.Nodes() {
		if !workflow.Terminal(name) {
			continue
		}
		judges++
		if len(succ[name]) > 0 {
			return fmt.Errorf("node %q (terminal gate) must be terminal, but it has successors: %v",
				name, succ[name])
		}
	}
	if judges != 1 {
		return fmt.Errorf("case must have exactly one terminal gate, got %d", judges)
	}
	return nil
}

func verdictFromTrace(workflow *dag.Dag, trace dag.Trace) map[string]any {
	for _, name := range workflow.Nodes() {
		if !workflow.Terminal(name) {
			continue
		}
		io := trace[name]
		if io.Output == nil {
			continue
		}
		if v, ok := io.Output["verdict"].(map[string]any); ok {
			return v
		}
	}
	return nil
}

// softFailures collects {node, detail} records from any node that recorded a soft
// gate failure (gate: soft / observe mode) — surfaced on every result so an
// observe run reads as a measurement, never silent green. Sorted by node for
// deterministic output.
func softFailures(workflow *dag.Dag, trace dag.Trace) []map[string]any {
	var out []map[string]any
	for _, name := range workflow.Nodes() {
		io, ok := trace[name]
		if !ok || io.Output == nil {
			continue
		}
		if gf, ok := io.Output["gate_failed"].(map[string]any); ok && gf != nil {
			out = append(out, gf)
		}
	}
	return out
}

// ── artifact record + printing ──────────────────────────────────────────────--

// nodeMetric collects {node: value} for every node whose output carries a truthy
// `key` (the attempts / duration_s regression signals).
func nodeMetric(trace dag.Trace, key string) map[string]any {
	out := map[string]any{}
	for node, io := range trace {
		if io.Output == nil {
			continue
		}
		if v, ok := io.Output[key]; ok && boolish(v) && !isZeroNum(v) {
			out[node] = v
		}
	}
	return out
}

func isZeroNum(v any) bool {
	switch x := v.(type) {
	case int:
		return x == 0
	case float64:
		return x == 0
	}
	return false
}

// ResultRecord is a JSON-safe per-run summary (no full trace) for trend tracking:
// rising attempts/durations across runs is a regression signal even when green.
func ResultRecord(res Result) map[string]any {
	schemaVersion := res.SchemaVersion
	if schemaVersion == 0 {
		schemaVersion = 2
	}
	return map[string]any{
		"schema_version": schemaVersion, "run_id": res.RunID,
		"ts": round2(float64(time.Now().UnixNano()) / 1e9), "started_at": res.StartedAt, "finished_at": res.FinishedAt,
		"id": res.ID, "status": res.Status,
		"failed_node": res.FailedNode, "detail": redactEvidence(res.Detail), "verdict": redactedRecordMap(res.Verdict),
		"cleanup_status": res.CleanupStatus, "cleanup_detail": redactEvidence(res.CleanupDetail),
		"cleanup_claims": res.CleanupClaims,
		"artifact_path":  res.ArtifactPath, "provenance": redactedRecordMap(res.Provenance),
		"soft_failures": redactedRecordMaps(orEmpty(res.SoftFailures)),
		"attempts":      nodeMetric(res.Trace, "attempts"),
		"durations":     nodeMetric(res.Trace, "duration_s"),
	}
}

func orEmpty(s []map[string]any) []map[string]any {
	if s == nil {
		return []map[string]any{}
	}
	return s
}

// PrintResult writes a human-readable summary (+ evidence on failure).
func PrintResult(res Result) {
	fmt.Printf("\n=== %s: %s ===\n", res.ID, res.Status)
	if res.FailedNode != "" || res.Detail != "" {
		fmt.Printf("  failed node: %s  detail: %s\n", res.FailedNode, redactEvidence(res.Detail))
	}
	if v := res.Verdict; v != nil {
		fmt.Printf("  verdict: pass=%v  %v\n", v["pass"], redactEvidence(asString(v["explanation"])))
	}
	for _, sf := range res.SoftFailures {
		fmt.Printf("  SOFT GATE FAILED (recorded, not fatal): [%v] %v\n",
			sf["node"], redactEvidence(asString(sf["detail"])))
	}
	if attempts := nodeMetric(res.Trace, "attempts"); len(attempts) > 0 {
		fmt.Printf("  attempts: %s\n", fmtMetric(attempts))
	}
	if durations := nodeMetric(res.Trace, "duration_s"); len(durations) > 0 {
		fmt.Printf("  durations(s): %s\n", fmtMetric(durations))
	}
	if res.Status != StatusPass {
		fmt.Println("  --- evidence ---")
		for _, node := range sortedTraceNodes(res.Trace) {
			io := res.Trace[node]
			out := io.Output
			if out != nil {
				so := strings.ReplaceAll(asString(out["stdout"]), "\n", " ")
				fmt.Printf("  [%s] exit=%v %s\n", node, out["exit_code"], truncate(redactEvidence(so), 300))
			} else if io.Err != "" {
				fmt.Printf("  [%s] error=%s\n", node, redactEvidence(io.Err))
			}
		}
	}
}

func PrintCleanupResult(io dag.NodeIO, detail string) {
	status := "pass"
	if detail != "" {
		status = "failed"
	}
	fmt.Printf("\n=== final-cleanup: %s ===\n", status)
	if detail != "" {
		fmt.Printf("  detail: %s\n", redactEvidence(detail))
	}
	if out := io.Output; out != nil {
		if stdout := strings.TrimSpace(asString(out["stdout"])); stdout != "" {
			fmt.Printf("  stdout: %s\n", truncate(redactEvidence(strings.ReplaceAll(stdout, "\n", " ")), 300))
		}
		if stderr := strings.TrimSpace(asString(out["stderr"])); stderr != "" && detail == "" {
			fmt.Printf("  stderr: %s\n", truncate(redactEvidence(strings.ReplaceAll(stderr, "\n", " ")), 300))
		}
		fmt.Printf("  duration(s): cleanup=%v\n", out["duration_s"])
	}
}

var evidenceRedactions = []struct {
	pattern *regexp.Regexp
	repl    string
}{
	{regexp.MustCompile(`(?i)("(?:user_key|api_key)"\s*:\s*")[^"]+(")`), `${1}<redacted>${2}`},
	{regexp.MustCompile(`(?i)(OPENVIKING_API_KEY=)[^\s"]+`), `${1}<redacted>`},
	{regexp.MustCompile(`(?i)(--api-key(?:=|\s+))[^\s"]+`), `${1}<redacted>`},
}

func redactEvidence(s string) string {
	for _, r := range evidenceRedactions {
		s = r.pattern.ReplaceAllString(s, r.repl)
	}
	return s
}

func redactedRecordMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = redactedRecordValue(v)
	}
	return out
}

func redactedRecordMaps(items []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, redactedRecordMap(item))
	}
	return out
}

func redactedRecordValue(v any) any {
	switch x := v.(type) {
	case string:
		return redactEvidence(x)
	case map[string]any:
		return redactedRecordMap(x)
	case []any:
		out := make([]any, 0, len(x))
		for _, item := range x {
			out = append(out, redactedRecordValue(item))
		}
		return out
	case []map[string]any:
		return redactedRecordMaps(x)
	default:
		return v
	}
}

func fmtMetric(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func sortedTraceNodes(trace dag.Trace) []string {
	keys := make([]string, 0, len(trace))
	for k := range trace {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
