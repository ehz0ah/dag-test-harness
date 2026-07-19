# Hermes Agent OpenViking Tests

These cases verify Hermes against a local OpenViking server using checkout-local
binaries and isolated runtime state. They are meant to run as ordinary `ovtest`
commands once the local services and config are in place; Codex is not required
to interpret the result.

## What These Tests Prove

- Hermes can write normal conversation turns into OpenViking through session
  commit and recall them in a fresh Hermes session.
- Hermes can recall prefetched OpenViking memory without calling memory tools.
- Hermes can use the OpenViking tools exposed by the Hermes agent:
  `viking_remember`, `viking_search`, `viking_read`, `viking_browse`,
  `viking_forget`, and `viking_add_resource`.
- The harness resets local Hermes/OpenViking runtime state between isolated
  runs, so stale Hermes session search or stale OpenViking storage should not
  satisfy a case by accident.

## Required Local Pieces

Use checkout-local binaries from the loop workspace:

```sh
cd /path/to/loop/OpenViking
uv sync

cd /path/to/loop/hermes-agent
uv sync
```

Required paths:

- `/path/to/loop/OpenViking/.venv/bin/ov`
- `/path/to/loop/OpenViking/.venv/bin/openviking-server`
- `/path/to/loop/hermes-agent/.venv/bin/hermes`
- `/path/to/loop/.ovtest/openviking/ov.conf`
- `/path/to/loop/.ovtest/openviking/ovcli.conf`
- `/path/to/loop/.ovtest/openviking/ovcli.conf.root`
- `/path/to/loop/.ovtest/hermes/template/config.yaml`
- `/path/to/loop/.ovtest/hermes/template/.env` if Hermes model config needs env
  values

`ov.conf` must point OpenViking storage inside `OV_TEST_STATE_DIR`, for example
`/path/to/loop/.ovtest/openviking/workspace`. `reset-local-state` refuses to
delete an OpenViking workspace outside the ovtest state directory.

`ovcli.conf.root` must contain the local server URL and `root_api_key`.
`ovcli.conf` must contain the same URL. Its `api_key` can be stale after a
reset because `bootstrap-openviking-user` rewrites it.

## Environment

Copy the example env file and export it with `set -a` so variables are visible
to the OpenViking server and Hermes subprocesses:

```sh
cd /path/to/loop/ovtest
cp .env.example /path/to/loop/.ovtest/.env

set -a
. /path/to/loop/.ovtest/.env
set +a
```

Minimum values:

```sh
export ARK_API_KEY=replace-with-real-key
export ARK_BASE_URL=https://ark.cn-beijing.volces.com/api/v3
export ARK_MODEL=doubao-seed-evolving
export OVTEST_JUDGE_MODEL=glm-5-2-260617

export OV_TEST_STATE_DIR=/path/to/loop/.ovtest
export OV_TEST_CONF_DIR=/path/to/loop/.ovtest/openviking
export OV_TEST_OPENVIKING_CONF=/path/to/loop/.ovtest/openviking/ov.conf
export OV_TEST_OV_BIN=/path/to/loop/OpenViking/.venv/bin/ov
export OV_TEST_HERMES_BIN=/path/to/loop/hermes-agent/.venv/bin/hermes
export OV_TEST_HERMES_HOME_TEMPLATE=/path/to/loop/.ovtest/hermes/template

export OV_TEST_ACCOUNT_ID=loop-ovtest
export OV_TEST_USER_ID=hermes
export OV_TEST_USER_KEY_SEED=loop-ovtest-hermes
```

The ARK key is used by OpenViking runtime model calls, OpenViking embedding
calls, Hermes model calls when configured that way, and LLM judge calls. The
judge model is deliberately separate from `ARK_MODEL` so judge behavior can
change without changing the product path under test.

## Fresh Local Run

Use this sequence for a clean local run:

```sh
cd /path/to/loop/ovtest

# Stop any local OpenViking server that points at this state dir first.
go run ./cmd/ovtest reset-local-state
```

Start OpenViking in a separate shell with the same exported env:

```sh
/path/to/loop/OpenViking/.venv/bin/openviking-server --config "$OV_TEST_OPENVIKING_CONF"
```

Then bootstrap the test user key:

```sh
cd /path/to/loop/ovtest
go run ./cmd/ovtest bootstrap-openviking-user
```

This command uses the root key to run `ov --sudo admin create-account`. If the
account already exists, it regenerates the user key. It writes the resulting
`result.user_key` into `ovcli.conf` and `ovcli.conf.root`, preserves
`root_api_key`, and prints only the key length.

Run a case:

```sh
go run ./cmd/ovtest hermes-openviking-sync-turn
```

For strict per-case isolation, repeat the stop, reset, start, bootstrap, and
case command for each case.

## Cases

```sh
go run ./cmd/ovtest hermes-openviking-sync-turn
go run ./cmd/ovtest hermes-openviking-prefetch-no-tools
go run ./cmd/ovtest hermes-openviking-tool-remember
go run ./cmd/ovtest hermes-openviking-tool-search-read-browse
go run ./cmd/ovtest hermes-openviking-tool-forget
go run ./cmd/ovtest hermes-openviking-tool-add-local-resource
go run ./cmd/ovtest hermes-openviking-tool-add-remote-resource
```

Case intent:

- `hermes-openviking-sync-turn`: ordinary story turn, Hermes session-end commit,
  OpenViking extraction, fresh-session recall, and LLM judge verdict.
- `hermes-openviking-prefetch-no-tools`: OpenViking memory is seeded first;
  Hermes runs with memory tools disabled and must answer from prefetch context.
- `hermes-openviking-tool-remember`: Hermes must call `viking_remember`, then
  OpenViking retrieval must find the stored fact.
- `hermes-openviking-tool-search-read-browse`: Hermes must call browse, search,
  and read. Missing tool evidence fails with the missing tool name.
- `hermes-openviking-tool-forget`: Hermes remembers a fact, searches for its
  URI, forgets that URI, and retrieval must no longer find it.
- `hermes-openviking-tool-add-local-resource`: Hermes adds the committed local
  fixture file at `fixtures/resources/agent-memory.md` through
  OpenViking and answers from indexed content.
- `hermes-openviking-tool-add-remote-resource`: Hermes adds a public URL through
  OpenViking and answers from indexed content.

## Remote Resource Fixture

The remote resource case defaults to:

```sh
export OV_TEST_REMOTE_RESOURCE_URL=https://fastly.jsdelivr.net/gh/ehz0ah/Celeste@5b8ab7f10d104e23ad12db68dc228e1606a6bcb6/README.md
export OV_TEST_REMOTE_RESOURCE_EXPECT="celeste fpga,basys3 fpga board"
```

Prefer a project-owned public fixture when possible:

```sh
export OV_TEST_REMOTE_RESOURCE_URL=https://your-public-fixture.example/hermes-ov.txt
export OV_TEST_REMOTE_RESOURCE_EXPECT="unique token,expected phrase"
```

OpenViking server-side remote ingestion only accepts reachable public URLs.
Local files are covered by `hermes-openviking-tool-add-local-resource`.

## Troubleshooting

- `reset-local-state` says the endpoint is reachable: stop the local
  `openviking-server` process before deleting file-backed state.
- OpenViking returns `403` or rejects the API key after reset: start the server,
  then run `go run ./cmd/ovtest bootstrap-openviking-user`. A storage reset
  recreates the key store, so preserved user keys become stale.
- OpenViking returns an ARK auth error: load `.env` with `set -a` or put real
  values directly in `ov.conf`. Shell variables that are not exported are not
  visible to `openviking-server`.
- Hermes appears to reuse old sessions: check that `OV_TEST_HERMES_HOME_TEMPLATE`
  contains only reusable config such as `config.yaml` and `.env`. The harness
  copies only those files into isolated per-case homes.
- Judge calls are slow or unstable: tune `OVTEST_JUDGE_MODEL`,
  `OVTEST_JUDGE_TIMEOUT`, and `OVTEST_JUDGE_RETRIES`. These settings affect the
  judge gate only, not the Hermes/OpenViking runtime model.
