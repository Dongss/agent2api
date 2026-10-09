# Where this fixture comes from

`simple.stream.jsonl` is a recording of a real `claude` run in headless print
mode, recorded on Claude Code `2.1.295`, captured with the argv the adapter
itself builds:

```sh
claude --print --output-format stream-json --verbose --tools "" \
       --include-partial-messages --no-session-persistence \
       --disable-slash-commands --strict-mcp-config \
       --thinking-display summarized \
       --setting-sources "" --model opus --system-prompt ""
```

The prompt goes on stdin. Three flags are worth calling out:

- `--system-prompt ""` is the default `replace` mode: no system prompt at all,
  which is what makes the backend answer as a plain model instead of an agent.
- `--model opus` is what the adapter passes for a `claude-code:opus` request.
  It is in the recording because the model has to be pinned for the assertions
  to mean anything — the bare `claude-code` model name lets the CLI choose, and
  a recording of whatever it chose that day would drift.
- `--thinking-display summarized` is not in `--help`; the adapter finds it with
  the probe described in `claudecode.go`. Without it this CLI sends every
  thinking block with its text empty (see History), and the recording would
  show the shape of a thinking block with nothing in it.

Opus thinks only when the prompt calls for it — asked to just repeat a line it
answered without a thinking block, twice — so the prompt gives it something to
work out whose answer is still the fixture's line:

```
Work out how many primes lie between 100 and 150. If there are exactly ten,
reply with exactly: Hello from the fixture. Otherwise reply with exactly:
Goodbye. Output nothing else.
```

The run resolved to `claude-opus-5-5`, answered `Hello from the fixture.`, and
reported `input_tokens: 2` / `output_tokens: 50` (39 of them thinking) with
`cache_creation_input_tokens: 662` and `stop_reason: end_turn`.

## What it pins

- `system` / `init` — the model name the CLI resolved, `tools: []` (proof the
  tool list really is empty), and the `cwd` it ran in.
- `system` / `status` and `system` / `thinking_tokens` — progress lines that
  carry no content and must not reach the caller. There are ten of the latter.
- `stream_event` wrapping the Anthropic wire events: `message_start`,
  `content_block_start` / `_delta` / `_stop` for **both** a `thinking` block and
  a `text` block, `message_delta` with `stop_reason`, `message_stop`. The
  thinking deltas carry text, which `TestGoldenTranscript` asserts: that
  assertion is what fails if `--thinking-display` stops reaching the CLI.
- `assistant` — whole-message lines that repeat content already streamed as
  deltas, one per content block. Dropping them is the reason the parser exists;
  without that, every answer would reach the caller twice. That is what
  `TestGoldenTranscript`'s duplicate check covers.
- `rate_limit_event` — quota state, ignored.
- `result` — `usage` with `input_tokens` / `output_tokens` /
  `cache_read_input_tokens` / `cache_creation_input_tokens` and
  `output_tokens_details.thinking_tokens`, plus `stop_reason`. The same line
  carries seven more `usage` keys the parser does not read (`cache_creation`,
  `fallback_credit`, `inference_geo`, `iterations`, `server_tool_use`,
  `service_tier`, `speed`); they are kept as recorded so a future parser that
  wants one can see the real shape.

A run that answers without thinking produces no `thinking` content block and no
`system` / `thinking_tokens` lines, which drops that side of the coverage. If a
re-recording comes back without them, prompt for reasoning and record again.

## The edits

The transcript is byte-for-byte as recorded except for these, all textual:

1. Thirty-six `session_id` / `uuid` values were real identifiers from a real
   call. They are opaque and authenticate nothing, but there is no reason to
   publish them, so they read `00000000-0000-4000-8000-00000000000N`, numbered
   in order of first appearance — the same shape, obviously synthetic.
2. The message id reads `msg_000000000000000000000004` — in the message
   itself and in the `api_message_id` 2.1.295 adds to every `stream_event`, 21
   lines in all — and the upstream `request_id` reads
   `req_000000000000000000000005`.
3. The `signature_delta` on the thinking block held a real thinking signature.
   It now reads a base64 string that decodes to a note saying it is synthetic.
   agent2api never reads this field.
4. `cwd` was the recording machine's temp directory. It reads
   `/tmp/agent2api-scratch`, the shape the gateway actually produces. It
   embedded a UUID of its own, so it has to be replaced before the UUIDs are
   renumbered, or the path is left half-scrubbed.
5. `memory_paths.auto` named a real home directory and the machine's private
   temp-folder id. It reads `/home/example/.claude/projects/…`, matching how the
   previous recording carried this field.
6. `messaging_socket_path` embedded the CLI process's pid. It reads `null`,
   which is what a run without a socket reports.

`rate_limit_event` carries the account's quota utilization and reset times. It
identifies nothing, and is kept as recorded.

Items 5 and 6 are worth naming explicitly: they are host-identifying rather than
call-identifying, they sit inside a long `system`/`init` line where they are easy
to miss, and a scrub that only looks at obvious id fields will walk straight past
them. Re-scan every string value, not a list of key names.

No event was added, removed or reordered, and no field was dropped: every edit
above replaces one string with another of the same shape.

## Re-recording

An account is all it takes: run the argv above with the prompt above in an empty
directory, check the run produced a thinking block with text in it, scrub the
six things listed under "The edits", and replace the file. The assertions in
`parse_test.go` pin this run's model name and answer text, so they will need the
new run's values; the token counts are asserted as non-zero rather than exactly,
so those survive a re-record.

## History

Recorded on 2.1.295 (2026-10-09) with Opus 5.5, replacing a Haiku recording
made on 2.1.231 (2026-08-25) and re-checked on 2.1.236, 2.1.267, 2.1.274 and
2.1.278. Haiku was the cheapest model that thinks; the switch to Opus is a
choice of which model the tests run against, not something the CLI forced.

Somewhere between 2.1.278 and 2.1.283 the CLI's print-mode default for thinking
became `thinking_display: "updates"`. Every thinking block still arrives, but
`thinking_delta.thinking` is `""` beside an `estimated_tokens` count, and the
whole-message `assistant` line's `thinking` is empty too. Nothing failed: the
parser skips empty deltas, so callers simply stopped receiving reasoning. The
Haiku recording could not catch it, having been made before the change. That is
why this one is recorded with `--thinking-display summarized` and why its
thinking text is asserted. 2.1.283, 2.1.293, 2.1.294 and 2.1.295 all accept the
flag.

Other changes between 2.1.278 and 2.1.295, none of them in a field the parser
reads:

- `system`/`thinking_tokens`, gone since 2.1.267, is back.
- Every `stream_event` carries `api_message_id`; thinking events carry
  `thinking_display`, and `thinking_delta` an `estimated_tokens`.
- `system`/`init` gained `plugins` (four built-ins, none adding a tool — `tools`
  is still `[]`), `per_turn_effort_active` and `view_mode`.
- `result` gained `safety_stops`, and `usage.fallback_credit`.
- `assistant` thinking lines carry `thinking_duration_ms`.

Earlier: since 2.1.236 the CLI stopped emitting `system`/`thinking_tokens` at
2.1.267, and its `result` line gained `first_content_frame_ms`,
`queued_turn_count` and `subagent_stats` (2.1.267) and `result_index` (2.1.274).
