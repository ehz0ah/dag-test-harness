package ops

// Gate / judge error taxonomy. These types drive status classification in
// runner.go: an op that hits a deterministic gate returns *GateFail (HARD →
// deterministic_check_failed); the judge returns *JudgeError only on a model/parse
// failure (a FAIL verdict is data, not an error).

// GateFail signals a HARD deterministic-gate failure: it fails the run fast and
// the judge never runs. Carries the node and the reason. (Mirrors the kernel's
// fail-fast: dag wraps it in *NodeError, but the original survives
// errors.As, so the runner can classify it.)
type GateFail struct {
	Node   string
	Detail string
}

func (e *GateFail) Error() string { return e.Node + ": " + e.Detail }

// ConfigError is an authoring/precondition error (a missing required config key,
// a missing env precondition). It classifies like any HARD gate
// (deterministic_check_failed) but is NEVER softened by the gate-severity knob or
// observe mode — a typo'd case must fail loud, not become a recorded "wobble".
type ConfigError struct{ *GateFail }

func (e *ConfigError) Unwrap() error { return e.GateFail }

func gateErr(node, detail string) *GateFail { return &GateFail{Node: node, Detail: detail} }

func configErr(node, detail string) *ConfigError {
	return &ConfigError{GateFail: &GateFail{Node: node, Detail: detail}}
}

// JudgeError means the judge could not produce a verdict (the ARK call or the
// verdict parse failed). Distinct from a FAIL verdict, which is a valid result.
type JudgeError struct{ Detail string }

func (e *JudgeError) Error() string { return "judge: " + e.Detail }
