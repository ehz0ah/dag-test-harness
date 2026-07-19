# Codex OpenViking Testing

These are live e2e cases for the OpenViking Codex plugin. They run the real
`codex exec` CLI with the installed OpenViking plugin enabled.

## Cases

- `codex-openviking-automatic-memory`: normal Codex turn -> plugin Stop
  capture -> fresh Codex SessionStart commit -> OpenViking extraction -> fresh
  Codex automatic recall.
- `codex-openviking-mcp-tools`: one comprehensive MCP flow covering health,
  remember, recall, search, read, add_resource, list, grep, glob, forget, and
  find against both the configured public remote resource and the committed
  local fixture at `fixtures/resources/agent-memory.md`.

## Requirements

- `codex` is installed and authenticated.
- The OpenViking Codex plugin is installed and enabled.
- Hook trust can be bypassed for automation with
  `--dangerously-bypass-hook-trust`.
- `OV_TEST_CONF_DIR/ovcli.conf` points to the target OpenViking server with a
  user API key.

The adapter writes a per-run `ovcli.conf` and forces plugin credentials through
the CLI credential source for each subprocess:

- `OPENVIKING_CREDENTIAL_SOURCE=cli`
- `OPENVIKING_CLI_CONFIG_FILE`
- `OPENVIKING_URL` / `OPENVIKING_BASE_URL`
- `OPENVIKING_API_KEY`
- `OPENVIKING_AUTH_MODE=api_key`
- `OPENVIKING_CODEX_STATE_DIR`

The state dir is isolated under `OV_TEST_STATE_DIR/codex/...` per case, so the
test does not use the user's normal Codex OpenViking plugin state.

## Run

```sh
go run ./cmd/ovtest codex-openviking-automatic-memory
go run ./cmd/ovtest codex-openviking-mcp-tools
```

Optional overrides:

```sh
export OV_TEST_CODEX_BIN=/path/to/codex
export OV_TEST_CODEX_MODEL=...
export OV_TEST_CODEX_TIMEOUT=900
export OV_TEST_CODEX_OPENVIKING_URL=http://127.0.0.1:1933
export OV_TEST_CODEX_OPENVIKING_API_KEY=...
export OV_TEST_CODEX_RESOURCE_URL=https://fastly.jsdelivr.net/gh/ehz0ah/Celeste@5b8ab7f10d104e23ad12db68dc228e1606a6bcb6/README.md
export OV_TEST_CODEX_RESOURCE_EXPECT='celeste fpga,basys3 fpga board'
```

The MCP tools case uses `OV_TEST_CODEX_RESOURCE_URL` /
`OV_TEST_CODEX_RESOURCE_EXPECT` when set, otherwise it uses the shared remote
resource defaults `OV_TEST_REMOTE_RESOURCE_URL` and
`OV_TEST_REMOTE_RESOURCE_EXPECT`. The default URL is the project-owned public
Celeste README fixture on GitHub.

The same MCP case also covers local file ingestion. It passes the committed
fixture file path to MCP `add_resource`, then lets Codex perform the required
signed `temp_upload` POST with `curl`. This exercises the real local-file path
instead of treating it as equivalent to remote URL ingestion.
