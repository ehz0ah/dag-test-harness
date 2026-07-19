package ops

import (
	"context"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/internal/cli"
)

// CleanupClaimsOutput is the reserved node-output field used to pass explicit
// resource ownership to the release executor. Claims are typed values produced
// by the operation that verified creation; arbitrary strings in trace output are
// never interpreted as cleanup authority.
const CleanupClaimsOutput = "cleanup_claims"

// CleanupClaim authorizes deletion of one exact URI created by the current run.
// Source is filled by the runner from the producing DAG node.
type CleanupClaim struct {
	URI    string `json:"uri"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Proof  string `json:"proof"`
}

// CLIResult is the public subprocess result shape for agent adapter packages.
type CLIResult = cli.Result

// cliResult keeps white-box tests and in-package helpers readable while the
// concrete subprocess implementation lives in internal/cli.
type cliResult = CLIResult

// ExecFunc is the execution closure used by adapter packages.
type ExecFunc func(in map[string]any) (map[string]any, error)

// OpContext exposes the small subset of root harness machinery that agent
// adapter packages need without moving the runner or OpenViking ops.
type OpContext struct{ b *base }

// NewFactory builds a DAG op factory for adapter packages.
func NewFactory(meta dag.Meta, critical bool, mk func(*OpContext) ExecFunc) dag.Factory {
	return factory(meta, critical, func(b *base) execFn {
		return execFn(mk(&OpContext{b: b}))
	})
}

func (c *OpContext) Name() string             { return c.b.name }
func (c *OpContext) Config() map[string]any   { return c.b.oc }
func (c *OpContext) Context() context.Context { return c.b.context() }

func (c *OpContext) Need(key string) (any, error) { return c.b.need(key) }
func (c *OpContext) NeedStr(key string) (string, error) {
	return c.b.needStr(key)
}
func (c *OpContext) OK(r CLIResult) error { return c.b.ok(r) }
func (c *OpContext) Poll(attempt func(last bool) (map[string]any, error), retry int) (map[string]any, int, error) {
	return c.b.poll(attempt, retry)
}

func (c *OpContext) RunCLI(argv []string, envExtra map[string]string, settle, timeout int) CLIResult {
	return RunCLIContext(c.b.context(), argv, envExtra, settle, timeout)
}

func (c *OpContext) RunOv(args []string, conf string, settle int) CLIResult {
	return RunOvContext(c.b.context(), args, conf, settle)
}

func (c *OpContext) GateErr(detail string) *GateFail {
	return gateErr(c.b.name, detail)
}

func (c *OpContext) ConfigErr(detail string) *ConfigError {
	return configErr(c.b.name, detail)
}

func GateError(node, detail string) *GateFail { return gateErr(node, detail) }
func ConfigErrorFor(node, detail string) *ConfigError {
	return configErr(node, detail)
}

// RunCLI is the adapter-facing subprocess hook. Tests may stub it.
var RunCLI = func(argv []string, envExtra map[string]string, settle, timeout int) CLIResult {
	return cli.Run(argv, envExtra, settle, timeout)
}

var RunCLIContext = func(ctx context.Context, argv []string, envExtra map[string]string, settle, timeout int) CLIResult {
	return cli.RunContext(ctx, argv, envExtra, settle, timeout)
}

// RunOv is the adapter-facing OpenViking CLI hook. Tests may stub it.
var RunOv = func(args []string, conf string, settle int) CLIResult {
	return cli.RunOv(args, conf, settle)
}

var RunOvContext = func(ctx context.Context, args []string, conf string, settle int) CLIResult {
	return cli.RunOvContext(ctx, args, conf, settle)
}

var writeUserConf = func(key string) (string, error) {
	return cli.WriteUserConf(key)
}

var runOv = func(args []string, conf string, settle int) CLIResult {
	return RunOv(args, conf, settle)
}

var runOvContext = func(ctx context.Context, args []string, conf string, settle int) CLIResult {
	return RunOvContext(ctx, args, conf, settle)
}

func AsString(v any) string            { return asString(v) }
func AsInt(v any, def int) int         { return asInt(v, def) }
func AsBool(v any) bool                { return asBool(v) }
func AsStrings(v any) []string         { return asStrings(v) }
func EnvInt(key string, def int) int   { return envInt(key, def) }
func FirstNonEmpty(a, b string) string { return firstNonEmpty(a, b) }
func Truncate(s string, n int) string  { return truncate(s, n) }
func LowerAll(ss []string) []string    { return lowerAll(ss) }
func MissingTokens(s string, tokens []string) []string {
	return missingTokens(s, tokens)
}
func PresentTokens(s string, tokens []string) []string {
	return presentTokens(s, tokens)
}
func ReadCLIConf(path string) (map[string]any, error) { return cli.ReadConf(path) }
func UserBaseConf() string                            { return userBaseConf() }
func UserConf(node string, userKey any) (string, error) {
	return userConf(node, userKey)
}
func TargetURL() (string, error)               { return cli.TargetURL() }
func TargetAPIKey() (string, error)            { return cli.TargetAPIKey() }
func ContainsAll(s string, subs []string) bool { return containsAll(s, subs) }

func CLIFields(r CLIResult) map[string]any { return cli.Fields(r) }
func ExitDetail(r CLIResult) string        { return cli.ExitDetail(r) }

func rootConf() string     { return cli.RootConf() }
func userBaseConf() string { return cli.UserBaseConf() }
