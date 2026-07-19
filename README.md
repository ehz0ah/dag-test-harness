# ovtest

Local release gate for OpenViking integrations with OpenClaw, Hermes Agent,
Codex, Claude Code, OpenCode, and Pi. The primary suite exercises the common
automatic capture/extraction/recall and comprehensive tool contracts, plus the
integration-specific OpenClaw compaction, Claude subagent, and Pi takeover
contracts.

## Source Layout

- `cmd/ovtest`: CLI runner.
- `dag`: dependency graph executor used by every case.
- `runner`: case execution, status classification, reports, cleanup summaries,
  and case-authoring helpers.
- `ops`: shared op factory support and concrete OpenViking/check/fixture
  implementations.
- `ops/openviking`, `ops/checks`, `ops/fixtures`: semantic op entrypoints used
  by cases.
- `adapters/hermes`, `adapters/openclaw`, `adapters/codex`,
  `adapters/claude`, `adapters/opencode`, `adapters/pi`: agent subprocess adapters used by
  agent cases.
- `internal/cli`: subprocess execution, OpenViking config resolution, and temp
  per-user config cleanup.
- `internal/releasegate`: locked source preparation, isolated builds, target
  lifecycle, provenance, retention, and exact cleanup primitives.
- `cases`: registry only.
- `cases/openviking`: OpenViking-direct cases.
- `cases/agents/hermes`, `cases/agents/openclaw`, `cases/agents/codex`,
  `cases/agents/claude`, `cases/agents/opencode`, `cases/agents/pi`: agent-specific OpenViking
  integration cases.
- `cases/experiment`: opt-in/low-frequency OpenViking API cases.

## Release Gate

`ovtest release` is the canonical update/regression workflow. It:

1. resolves OpenViking from its canonical Git source and freezes the exact SHA;
2. installs the selected harnesses from their current official stable channels
   into a disposable prefix (npm for Codex, Claude Code, OpenClaw, OpenCode,
   and Pi; Hermes' official installer pinned to its resolved release commit);
3. records and verifies lockfiles, package identities, registry hosts,
   executable hashes, and canonical installed-tree hashes;
4. builds OpenViking in a disposable worktree without runtime credentials;
5. starts a fresh local OpenViking server in API-key mode on a random loopback
   port, or validates an existing external server;
6. validates each selected executable, authentication, and model before product
   cases run;
7. runs every selected case serially with an isolated home, config, session,
   queue-scope key, and runtime state;
8. emits immutable environment and per-attempt manifests plus redacted result
   evidence; and
9. stops the local service and discards its state, or deletes only exact
   run-owned external data, reporting primary and cleanup failures separately.

The default command runs the 21-case OpenViking suite:

```sh
go run ./cmd/ovtest release \
  --source latest \
  --target local \
  --trusted-execution \
  --env-file ../.env
```

Build/install prerequisites are `git`, `uv`, `node`, and `npm`. Persistent bare
mirrors and dependency caches are accelerators only; worktrees, install
prefixes, build outputs, runtime homes, and OpenViking state are disposable.

The OpenViking suite contains six OpenViking-owned service and semantic
contracts, the two common integration contracts (automatic
capture/extraction/recall and comprehensive tools) for all six harnesses, and
the three integration-specific lifecycle cases. Harness suites run only their
relevant cases:

```sh
go run ./cmd/ovtest -list-suites
go run ./cmd/ovtest release --suite codex --trusted-execution --env-file ../.env
go run ./cmd/ovtest release --suite hermes-agent --trusted-execution --env-file ../.env
```

The credential file is loaded only after candidate source is fetched, trust is
explicitly granted, credential-free builds finish, and official harnesses are
installed. It is used only for local OpenViking model/embedding providers and
the OpenClaw, Hermes, and Pi model provider. Codex, Claude Code, and OpenCode
reuse existing machine authentication; ovtest validates but never installs,
refreshes, or mutates that authentication.

Supported credential names are:

```text
OV_TEST_HARNESS_LLM_API_KEY     # shared by OpenClaw, Hermes, and Pi
OPENVIKING_LLM_API_KEY          # OPENVIKING_API_KEY is a legacy fallback
OPENVIKING_LLM_BASE_URL
OPENVIKING_LLM_MODEL
OPENVIKING_EMBEDDING_API_KEY    # OPENVIKING_API_KEY is a legacy fallback
OPENVIKING_EMBEDDING_BASE_URL
OPENVIKING_EMBEDDING_MODEL
OV_TEST_OPENVIKING_API_KEY      # external OpenViking user key only
```

`ARK_API_KEY`, `ARK_BASE_URL`, and `ARK_MODEL` remain accepted as legacy LLM
fallbacks. If `OV_TEST_HARNESS_LLM_API_KEY` is unset, harnesses reuse the
OpenViking LLM key for backward compatibility. New release environments should
use the explicit split names above.

For a Debian/Ubuntu VPS, `scripts/setup-ovtest-vps.sh` provisions the service
user and prerequisites, creates a protected credential template, and guides
the one-time Codex, Claude Code, and OpenCode logins. It never stores secrets in
the script; run it with `--help` for the three setup commands.

Provider base URLs must use the form expected by the frozen OpenViking source.
For the current Volcengine provider, use the API root (for example,
`https://ark.cn-beijing.volces.com/api/v3`), not a path ending in
`/responses` or `/embeddings/multimodal`.

The same whitelisted variables may be supplied explicitly in the command's
process environment; they override file values. No unrelated shell variables
are imported into typed runtime credentials.

Use an existing, usually remote, OpenViking server:

```sh
go run ./cmd/ovtest release \
  --source latest \
  --target external \
  --url https://openviking.example.com \
  --trusted-execution \
  --env-file ../.env
```

External mode is diagnostic unless the server can prove the exact candidate
build identity. The current health contract does not expose that identity, so
external results cannot qualify an OpenViking release.

Replay an exact retained environment (source, official package artifacts,
toolchain/platform identity, gate definition, plugins, and configuration):

```sh
go run ./cmd/ovtest release \
  --source manifest \
  --manifest "$HOME/.cache/ovtest/release-gate/runs/<run-id>/environment-manifest.json" \
  --target local \
  --trusted-execution \
  --env-file ../.env
```

`latest` resolves only current official stable packages. `manifest` reinstalls
the previously recorded exact stable artifacts; arbitrary harness versions,
repositories, branches, and PR overrides are intentionally not supported.
OpenViking candidate source may be selected explicitly:

```sh
go run ./cmd/ovtest release \
  --openviking-ref refs/pull/3232/head \
  --target local \
  --trusted-execution \
  --env-file ../.env
```

`--trusted-execution` is a deliberate security boundary. Candidate OpenViking
and plugin code runs with scoped test credentials. CI must set the flag only
after establishing trusted source provenance; do not use this command from a
credentialed `pull_request_target` job or for untrusted fork code.

Useful release flags:

- `--root`: persistent bare mirrors, dependency caches, manifests, and retained
  evidence; defaults to the user cache directory.
- `--suite`: `openviking`, `hermes-agent`, `openclaw`, `codex`, `claude-code`,
  `opencode`, `pi`, or `smoke`.
- `--qualification-runs`: fresh complete attempts against one frozen
  environment; defaults to one, while a release gate should use three.
- `--retention-days`: failed evidence and protected source ref retention;
  defaults to 30 days.
- `--keep-success-evidence`: retain full redacted successful evidence; compact
  manifests, results, and the summary are always retained.
- `--openviking-config`: override the bundled local Volcengine-compatible JSON
  template.
- `--codex-model`, `--claude-model`, `--opencode-model`: optional explicit
  model selections; otherwise the respective CLI default is used.

Secret directories and the environment-scoped queue HMAC key are always
deleted. Successful runtime and full evidence are deleted unless explicitly
retained. Failed runtime and redacted evidence plus immutable manifests are
retained for diagnosis. The queue key is shared by all processes within one
isolated attempt, never stored in a per-process or replaceable plugin directory,
and never survives cleanup.

Every attempt gets a new OpenViking storage directory and random loopback port;
teardown stops that service and the state is never reused. Repeated
qualification attempts and candidate/base comparisons also use fresh target
and harness state. External runs
cannot discard the shared server, so they delete only exact memory/session
leaves and resource roots registered from structured run evidence. Broad
session, user, and resource namespaces are never cleanup targets.

Candidate/base comparison is attempted only for candidate-sensitive case
failures. Install, authentication, preflight, infrastructure, evidence-write,
and cleanup failures remain environment failures. When a frozen base passes,
the candidate is retried in another fresh environment: two candidate failures
with a base pass are classified as a probable OpenViking regression; a shared
failure is baseline incompatibility; mixed outcomes are flaky/inconclusive.

## Manual Case Setup

The lower-level commands below remain available for focused development runs
against an already configured OpenViking target. They do not prepare sources or
manage a service.

### Setup

```sh
export OV_TEST_CONF_DIR=$HOME/.openviking   # has ovcli.conf with url + api_key
export ARK_API_KEY=...
export ARK_MODEL=doubao-seed-evolving       # OpenViking/Hermes runtime model
export OVTEST_JUDGE_MODEL=glm-5-2-260617
```

For a loop-style workspace, keep volatile test state outside the repos:

```sh
export LOOP_DIR=/path/to/loop

cp .env.example "$LOOP_DIR/.ovtest/.env"
set -a
. "$LOOP_DIR/.ovtest/.env"
set +a

export OV_TEST_STATE_DIR="$LOOP_DIR/.ovtest"
export OV_TEST_CONF_DIR="$LOOP_DIR/.ovtest/openviking"
export OV_TEST_OPENVIKING_CONF="$LOOP_DIR/.ovtest/openviking/ov.conf"
export OV_TEST_OV_BIN="$LOOP_DIR/OpenViking/.venv/bin/ov"
export OV_TEST_HERMES_BIN="$LOOP_DIR/hermes-agent/.venv/bin/hermes"
export OV_TEST_HERMES_HOME_TEMPLATE="$LOOP_DIR/.ovtest/hermes/template"
export OV_TEST_ACCOUNT_ID=loop-ovtest
export OV_TEST_USER_ID=hermes
export OV_TEST_USER_KEY_SEED=loop-ovtest-hermes
export ARK_BASE_URL=https://ark.cn-beijing.volces.com/api/v3
export ARK_MODEL=doubao-seed-evolving
export OVTEST_JUDGE_MODEL=glm-5-2-260617
```

Optional:

- `OV_TEST_OV_BIN=/path/to/ov`
- `OV_TEST_CASE_TIMEOUT=1800`
- `OV_TEST_CLI_TIMEOUT=300`
- `OVTEST_GATE_MODE=observe`
- `OV_TEST_STATE_DIR=/path/to/scratch`
- `OV_TEST_OPENVIKING_CONF=/path/to/ov.conf`
- `OV_TEST_HERMES_BIN=/path/to/hermes`
- `OV_TEST_HERMES_HOME_TEMPLATE=/path/to/hermes-template-home`
- `OV_TEST_HERMES_TIMEOUT=300`
- `OV_TEST_CODEX_BIN=/path/to/codex`
- `OV_TEST_CODEX_MODEL=...`
- `OV_TEST_CODEX_TIMEOUT=900`
- `OV_TEST_CODEX_OPENVIKING_URL=...`
- `OV_TEST_CODEX_OPENVIKING_API_KEY=...`
- `OV_TEST_CLAUDE_BIN=/path/to/claude`
- `OV_TEST_CLAUDE_MODEL=...`
- `OV_TEST_CLAUDE_TIMEOUT=900`
- `OV_TEST_OPENCODE_BIN=/path/to/opencode`
- `OV_TEST_OPENCODE_MODEL=...`
- `OV_TEST_OPENCODE_TIMEOUT=1200`
- `OV_TEST_OPENCODE_PLUGIN_ROOT=/path/to/OpenViking/examples/opencode-plugin`
- `OV_TEST_OPENCODE_CONFIG_TEMPLATE=/path/to/opencode.json`
- `OV_TEST_OPENCODE_OPENVIKING_URL=...`
- `OV_TEST_OPENCODE_OPENVIKING_API_KEY=...`
- `OV_TEST_ACCOUNT_ID=loop-ovtest`
- `OV_TEST_USER_ID=hermes`
- `OV_TEST_USER_KEY_SEED=loop-ovtest-hermes`
- `OV_TEST_HERMES_OPENVIKING_ENDPOINT=...`
- `OV_TEST_HERMES_OPENVIKING_API_KEY=...`
- `OV_TEST_OPENCLAW_OPENVIKING_URL=...`
- `OV_TEST_OPENCLAW_OPENVIKING_API_KEY=...`
- `OV_TEST_OPENCLAW_BIN=/path/to/openclaw`
- `OV_TEST_OPENCLAW_GATEWAY_START_TIMEOUT_SECONDS=120`
- `OV_TEST_OPENCLAW_COMPACTION_TIMEOUT_SECONDS=540`
- `ARK_BASE_URL=...`
- `OVTEST_JUDGE_MODEL=glm-5-2-260617`
- `OVTEST_JUDGE_TIMEOUT=120`
- `OVTEST_JUDGE_RETRIES=1`
- `OV_TEST_CLEANUP_MODE=none|api`
- `OV_TEST_CLEANUP_URIS=viking://user/memories/<exact-id>,viking://resources/ovtest-runs/<run-id>`
- `OV_TEST_REMOTE_RESOURCE_URL=https://raw.githubusercontent.com/ehz0ah/Celeste/5b8ab7f10d10/README.md`
- `OV_TEST_REMOTE_RESOURCE_EXPECT=celeste fpga,basys3 fpga board`

`openclaw-memory` uses a preconfigured local OpenClaw CLI:
`openclaw agent --local`. Configure the OpenViking plugin to opt into per-run
env injection:

```sh
printf '%s\n' '{"plugins":{"entries":{"openviking":{"config":{"baseUrl":"${OPENVIKING_URL}","apiKey":"${OPENVIKING_API_KEY}","commitTokenThreshold":0}}}}}' \
  | openclaw config patch --stdin
openclaw gateway restart
```

All cases use the preconfigured `ovcli.conf` URL/API key. For the loop-local
workflow, reset file-backed test state before starting the local OpenViking
server:

```sh
go run ./cmd/ovtest reset-local-state
```

`reset-local-state` removes per-run Hermes homes, fixture state, and the
OpenViking `storage.workspace` from `OV_TEST_OPENVIKING_CONF`, while preserving
`OV_TEST_CONF_DIR/ov.conf`, `OV_TEST_CONF_DIR/ovcli.conf`, and the Hermes
template config. It refuses to run if the configured local OpenViking endpoint
is reachable, so stop the local server first.

After a storage reset, the OpenViking key store is fresh, so any preserved
`ovcli.conf` user API key is stale. Start the local OpenViking server, then run
the desired case normally; ovtest preflights local OpenViking-dependent cases
with `ov status` and automatically recreates the test account/user when the
local user key is rejected and a root config is available.

You can still run the repair directly when debugging setup:

```sh
go run ./cmd/ovtest bootstrap-openviking-user
```

Automatic preflight and `bootstrap-openviking-user` use `ov --sudo admin create-account` with
`OV_TEST_ACCOUNT_ID`, `OV_TEST_USER_ID`, and `OV_TEST_USER_KEY_SEED`, then writes
the returned `result.user_key` into both `ovcli.conf` and `ovcli.conf.root`
while preserving `root_api_key`. It does not print the key.

Normal case runs do not clean OpenViking through the API. For remote/shared
servers where local file reset is impossible, opt in explicitly with
`OV_TEST_CLEANUP_MODE=api` and set `OV_TEST_CLEANUP_URIS` to concrete test-owned
scopes. API cleanup fails closed when `OV_TEST_CLEANUP_URIS` is unset or contains
no safe concrete target. It never derives or deletes broad user/resource roots.
The managed `release` workflow instead tracks and removes exact run-owned URIs.

Rule: literal OpenClaw config wins; env only fills missing values or explicit
`${OPENVIKING_*}` placeholders. Do not rely on `~/.zshrc` for tests. ovtest
passes the preconfigured URL/API key only to the `openclaw agent --local` child
process. Set `OV_TEST_OPENCLAW_OPENVIKING_API_KEY` only when you intentionally
want OpenClaw to use a different OpenViking user.

Hermes/OpenViking cases use `hermes --cli chat -q ... --quiet` with isolated
`HERMES_HOME` directories under `OV_TEST_STATE_DIR/hermes`. ovtest injects the
OpenViking endpoint/API key into the Hermes subprocess, but the isolated Hermes
homes still need valid Hermes model/provider configuration. When
`OV_TEST_HERMES_HOME_TEMPLATE` is set, ovtest copies only `config.yaml` and
`.env` from that template directory into each isolated Hermes home; it does not
copy session databases or other runtime state.

See [Hermes Agent Testing](cases/agents/hermes/README.md) for the full local
Hermes/OpenViking setup and isolated-run workflow.

The Hermes cases are explicit-only and cover:

- `hermes-openviking-sync-turn`: normal story turn -> Hermes session-end commit
  -> OpenViking extraction -> fresh-session recall -> LLM judge.
- `hermes-openviking-prefetch-no-tools`: seeded OpenViking memory is recalled
  with Hermes memory tools disabled; evidence must not contain `viking_*` calls.
- `hermes-openviking-tool-remember`: Hermes writes through OpenViking remember.
- `hermes-openviking-tool-search-read-browse`: Hermes must browse, search, and
  read; missing tool evidence fails with the missing tool name.
- `hermes-openviking-tool-forget`: Hermes remembers, finds, forgets the concrete
  URI, and retrieval must no longer find it.
- `hermes-openviking-tool-add-local-resource`: Hermes ingests and retrieves the
  committed local fixture file at `fixtures/resources/agent-memory.md`.
- `hermes-openviking-tool-add-remote-resource`: Hermes ingests and retrieves a
  public remote URL with sufficient wait. Override `OV_TEST_REMOTE_RESOURCE_URL`
  and `OV_TEST_REMOTE_RESOURCE_EXPECT` to use a project-owned fixture.

Codex/OpenViking cases use `codex exec` with the installed OpenViking Codex
plugin. ovtest writes a per-run `ovcli.conf`, injects it into each Codex
subprocess, and isolates plugin state under `OV_TEST_STATE_DIR/codex`.
The cases intentionally bypass hook trust for
automation and are explicit-only because they launch real Codex/model calls.

See [Codex OpenViking Testing](cases/agents/codex/README.md) for the exact
contract and overrides.

The Codex cases are:

- `codex-openviking-automatic-memory`: real Codex auto-capture -> SessionStart
  commit -> OpenViking extraction -> fresh Codex automatic recall.
- `codex-openviking-mcp-tools`: one comprehensive MCP flow covering health,
  remember, find, search, read, add_resource, list, grep, glob, and forget
  against the configured public remote resource and the committed local
  fixture file at `fixtures/resources/agent-memory.md`.

OpenCode/OpenViking cases use `opencode run --auto --format json` with the
OpenViking OpenCode plugin source-installed into an isolated OpenCode config
directory per run. The adapter does not pass `--pure`, because that would disable
plugin/MCP loading. It writes isolated `ovcli.conf`, `ov.conf`,
`openviking-config.json`, OpenCode config/data/cache/state dirs, plugin runtime
state, and debug logs under `OV_TEST_STATE_DIR/opencode`.

Required local pieces:

- `opencode` 1.17+ or `OV_TEST_OPENCODE_BIN=/path/to/opencode`
- a model/provider configuration visible to OpenCode, or
  `OV_TEST_OPENCODE_MODEL=provider/model`
- `OV_TEST_OPENCODE_CONFIG_TEMPLATE=/path/to/opencode.json` when provider
  settings should be copied into the isolated OpenCode config dir
- `/path/to/loop/OpenViking/examples/opencode-plugin`, or
  `OV_TEST_OPENCODE_PLUGIN_ROOT=/path/to/opencode-plugin`
- `OV_TEST_CONF_DIR/ovcli.conf` pointing at the target OpenViking server with a
  user API key

The OpenCode cases are explicit-only because they launch real OpenCode/model
calls:

- `opencode-openviking-automatic-memory`: real OpenCode auto-capture -> plugin
  lifecycle commit -> OpenViking extraction -> fresh OpenCode automatic recall.
- `opencode-openviking-mcp-tools`: one comprehensive MCP flow covering health,
  remember, find, search, add_resource, list, glob, and forget against the
  configured public remote resource and the committed local fixture file at
  `fixtures/resources/agent-memory.md`.

Claude Code uses `claude -p --no-session-persistence` with the prepared plugin
source passed through `--plugin-dir` and user/project settings disabled. Its
primary cases are `claude-code-openviking-automatic-memory` and
`claude-code-openviking-mcp-tools`; the additional
`claude-code-openviking-subagent-lifecycle` case verifies SubagentStart/Stop
hooks, extraction, and fresh-run recall.

## Offline Check

```sh
go test ./cases
```

## Run Cases

```sh
go run ./cmd/ovtest -list

go run ./cmd/ovtest ov-memory
go run ./cmd/ovtest ov-memory-direct
go run ./cmd/ovtest ov-memory-update
go run ./cmd/ovtest ov-experience-learning
go run ./cmd/ovtest ov-negative-recall
go run ./cmd/ovtest ov-retrieval-precision
go run ./cmd/ovtest ov-forget-ghost
go run ./cmd/ovtest ov-search-compare
go run ./cmd/ovtest ov-memory-cjk
go run ./cmd/ovtest openclaw-memory
go run ./cmd/ovtest hermes-openviking-automatic-memory
go run ./cmd/ovtest hermes-openviking-tools
go run ./cmd/ovtest codex-openviking-automatic-memory
go run ./cmd/ovtest codex-openviking-mcp-tools
go run ./cmd/ovtest opencode-openviking-automatic-memory
go run ./cmd/ovtest opencode-openviking-mcp-tools
```

Experimental / low-frequency API cases:

```sh
go run ./cmd/ovtest experiment-ov-export-import
go run ./cmd/ovtest experiment-ov-reindex-semantic
go run ./cmd/ovtest experiment-ov-add-resource-folder
go run ./cmd/ovtest experiment-ov-write-refresh
go run ./cmd/ovtest experiment-ov-relations-link
```

`experiment-ov-export-import` and `experiment-ov-reindex-semantic` require
`OV_TEST_CONF_DIR/ovcli.conf` to point at an API key with admin role. Reindex
uses `--mode semantic_and_vectors`.

Run the 21-case primary release suite against the already configured target:

```sh
go run ./cmd/ovtest all
```

Keep the original quick two-case development lane (`ov-memory-direct`, legacy
alias `openclaw-memory`):

```sh
go run ./cmd/ovtest smoke
```

Run any other case by name from `-list`.

Repeat and report:

```sh
go run ./cmd/ovtest --repeat 5 --out runs.jsonl ov-memory
go run ./cmd/ovtest report runs.jsonl
go run ./cmd/ovtest report runs.jsonl --case ov-memory
```
