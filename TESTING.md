# ovtest testing framework

`ovtest` should stay small, but it needs a clear rule for what deserves a test.
The useful boundary is a contract that can break an OpenViking e2e run or make
its evidence misleading.

## Layers

1. DAG kernel contracts

   Cover graph authoring and execution semantics that every case depends on:
   declared inputs, declared outputs, condition dependencies, cycles, skipped
   branches, fan-in/select behavior, fail-fast, panic recovery, concurrency, and
   runtime output maps. These tests live in `dag/` and never call OpenViking.

2. Harness operator contracts

   Cover typed `ov` and OpenClaw operators with subprocesses stubbed: identity
   resolution, hard/soft gate severity, config validation, JSON schema gates,
   readiness polling, and capability-specific structural checks such as
   retrieval filters or session extraction readiness.

3. Runner and artifact contracts

   Cover status taxonomy and evidence shape: build errors, executor errors,
   deterministic gate failures, judge failures, semantic failures, soft-gate
   surfacing, terminal gate enforcement, local-state reset safety, compact
   JSONL records, and report attribution.

4. Case authoring smokes

   Every real case must build offline, be acyclic, and have exactly one terminal
   gate. That gate may be an LLM judge when semantic scoring is required, or a
   deterministic check when exact evidence is enough. This catches bad wiring in
   milliseconds without live services.

5. Live product-path cases

   Keep these sparse and boring. A live case is necessary only when it exercises
   a real OpenViking/OpenClaw product path that unit tests cannot prove:
   CLI JSON contracts, session commit and extraction, memory retrieval,
   negative recall, removal, CJK round trip, search/find comparison, or OpenClaw
   handoff.

## What to add

Add or update a test when a change introduces one of these:

- a new DAG semantic or a new way to wire data;
- a new operator capability or CLI JSON contract;
- a new deterministic gate or readiness loop;
- a new status/artifact field used by reports or trend tracking;
- a new live case that protects a distinct OpenViking product path.

Do not add tests for exact prose, judge prompt wording, private logs, raw live
outputs, or duplicated Go standard-library behavior. Prefer one readable test
at the contract boundary over many narrow implementation tests.

## Current contract priorities

- Runtime DAG output maps must include every declared `Meta.Outputs` key after a
  successful op run. Missing keys are execution bugs; present nil values are
  valid evidence.
- Report loading should make corrupt JSONL rows visible instead of silently
  dropping them.
- Local reset must delete only ovtest-owned runtime state and preserve
  reusable config. API cleanup is opt-in and must target concrete scopes.
- LLM judge configuration must be separate from the product runtime model so
  changing judge quality or latency does not change the system under test.
