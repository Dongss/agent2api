# Where this fixture comes from

`simple.jsonl` is a recording of a real `qwen` run, verified against Qwen Code
`0.24.0`, captured with the argv the adapter itself builds:

```sh
qwen --output-format stream-json
```

The prompt goes on stdin. `--prompt` exists and appends to stdin, but a
replayed conversation can exceed the OS argv limit, so stdin carries all of it.

The run was made in a scratch directory holding the `.qwen/settings.json` the
adapter generates — the file that leaves the CLI no tools. It reported
`model: qwen3.6-plus`, answered `Hello from the fixture.`, and used 20,407 input
/ 81 output tokens.

## What it pins

- `system` / `init` — the resolved model, the `cwd`, and **`tools: []`**. That
  last one is the containment check: `TestFixtureStartsWithNoTools` asserts the
  recording was made with no tools available, so a re-recording taken without
  the settings file fails rather than quietly becoming the golden case.
- `stream_event` — progress lines (`goal_state` and the like) that carry no
  content and must not reach the caller.
- `assistant` — whole messages, one line per content block, carrying `thinking`
  and `text`. This CLI does not stream partial text.
- `result` — the terminal line: `result`, `usage`
  (`input_tokens` / `output_tokens` / `cache_read_input_tokens`), `is_error`,
  `subtype`. It repeats the answer already sent as an `assistant` line, so the
  parser has to treat it as a duplicate rather than append it twice.

## Two things the recording cannot show

**Tools available.** Covered by `TestToolsAvailableFailsTheRun` from a
constructed transcript: recording it would mean deliberately running the CLI
uncontained, which is the one thing this adapter exists to prevent.

**An upstream API failure.** The CLI reports one as `is_error: false`,
`subtype: "success"`, `error: null`, with the error as the assistant's answer:

```
[API Error: 400 Access denied, please make sure your account is in good standing…]
```

There is no machine-readable signal, so the adapter matches that wrapper — see
`apiErrorIn` — and `TestUpstreamAPIErrorIsNotAnAnswer` pins it. This was found
by running against an account that had gone into arrears, not by reading the
docs; if a future release starts setting `is_error` properly, the text match
becomes redundant rather than wrong.

## The edits

The transcript is byte-for-byte as recorded except for these, all textual:

1. Five `session_id` / `uuid` values were real identifiers from a real call.
   They are opaque and authenticate nothing, but there is no reason to publish
   them, so they read `00000000-0000-4000-8000-00000000000N`.
2. `cwd` was the recording machine's temp directory. It reads
   `/tmp/agent2api-scratch`, the shape the gateway produces.

No event was added, removed or reordered, and no field was dropped.

Scrubbing was done by **scanning every string value** for anything UUID-shaped
or host-identifying, not by walking a list of key names. A first pass keyed on
names left two real UUIDs behind, under keys the list did not anticipate — the
same trap `claudecode/testdata/PROVENANCE.md` records.

## Re-recording

An authenticated install is all it takes. The settings file matters: run in an
empty directory containing `.qwen/settings.json` exactly as
`writeSettings` generates it, or the recording will carry a non-empty tool list
and the containment test will reject it.

Do **not** add `--safe-mode`. It disables customisations, and the settings file
is one — with it the CLI came back with all 23 tools live.

The assertions in `parse_test.go` pin this run's model name and answer text, so
they will need the new run's values; token counts are asserted as non-zero
rather than exactly, so those survive a re-record.

## History

Recorded on 0.22.3, re-checked on 0.23.2 and 0.24.0: no event type, `result` key or `usage`
key changed, and `system`/`init` still reports `tools: []` under the generated
settings — the containment survives the upgrade, which is the check that
matters. Neither re-check produced a reasoning block, which is ordinary variation
rather than a change; the recording keeps one.
