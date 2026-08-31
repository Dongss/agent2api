# Where this fixture comes from

`simple.stream.jsonl` is a recording of a real `claude` run in headless print
mode, verified against Claude Code `2.1.236`, captured with the argv the adapter
itself builds:

```sh
claude --print --output-format stream-json --verbose --tools "" \
       --include-partial-messages --no-session-persistence \
       --disable-slash-commands --strict-mcp-config \
       --setting-sources "" --system-prompt "" --model haiku
```

The prompt goes on stdin. Two flags are worth calling out:

- `--system-prompt ""` is the default `replace` mode: no system prompt at all,
  which is what makes the backend answer as a plain model instead of an agent.
- `--model haiku` is what the adapter passes for a `claude-code:haiku` request.
  It is in the recording because the model has to be pinned for the assertions
  to mean anything — the bare `claude-code` model name lets the CLI choose, and
  a recording of whatever it chose that day would drift. It also happens to be
  the cheapest way to get a run that thinks.

The run resolved to `claude-haiku-4-5-20251001`, answered
`Hello from the fixture.`, and reported `input_tokens: 163` /
`output_tokens: 85` with `stop_reason: end_turn`.

## What it pins

- `system` / `init` — the model name the CLI resolved, `tools: []` (proof the
  tool list really is empty), and the `cwd` it ran in.
- `system` / `status` and `system` / `thinking_tokens` — progress lines that
  carry no content and must not reach the caller. There are five of the latter.
- `stream_event` wrapping the Anthropic wire events: `message_start`,
  `content_block_start` / `_delta` / `_stop` for **both** a `thinking` block and
  a `text` block, `message_delta` with `stop_reason`, `message_stop`.
- `assistant` — whole-message lines that repeat content already streamed as
  deltas, one per content block. Dropping them is the reason the parser exists;
  without that, every answer would reach the caller twice. That is what
  `TestGoldenTranscript`'s duplicate check covers.
- `rate_limit_event` — quota state, ignored.
- `result` — `usage` with `input_tokens` / `output_tokens` /
  `cache_read_input_tokens` / `cache_creation_input_tokens`, plus `stop_reason`.
  The same line carries seven more `usage` keys the parser does not read
  (`cache_creation`, `inference_geo`, `iterations`, `output_tokens_details`,
  `server_tool_use`, `service_tier`, `speed`); they are kept as recorded so a
  future parser that wants one can see the real shape.

A run that answers without thinking produces no `thinking` content block and no
`system` / `thinking_tokens` lines, which drops that side of the coverage. If a
re-recording comes back without them, prompt for reasoning and record again.

## The edits

The transcript is byte-for-byte as recorded except for these, all textual:

1. Twenty-five `session_id` / `uuid` values were real identifiers from a real
   call. They are opaque and authenticate nothing, but there is no reason to
   publish them, so they read `00000000-0000-4000-8000-00000000000N`, numbered
   in order of first appearance — the same shape, obviously synthetic.
2. The message `id` reads `msg_000000000000000000000004` and the upstream
   `request_id` reads `req_000000000000000000000005`.
3. The `signature_delta` on the thinking block held a real thinking signature.
   It now reads a base64 string that decodes to a note saying it is synthetic.
   agent2api never reads this field.
4. `cwd` was the recording machine's temp directory. It reads
   `/tmp/agent2api-scratch`, the shape the gateway actually produces.
5. `memory_paths.auto` named a real home directory and the machine's private
   temp-folder id. It reads `/home/example/.claude/projects/…`, matching how the
   previous recording carried this field.
6. `messaging_socket_path` embedded the CLI process's pid. It reads `null`,
   which is what a run without a socket reports.

Items 5 and 6 are worth naming explicitly: they are host-identifying rather than
call-identifying, they sit inside a long `system`/`init` line where they are easy
to miss, and a scrub that only looks at obvious id fields will walk straight past
them. Re-scan every string value, not a list of key names.

No event was added, removed or reordered, and no field was dropped: every edit
above replaces one string with another of the same shape.

## Re-recording

An account is all it takes: run the argv above in an empty directory, scrub the
six things listed under "The edits", and replace the file. The assertions in
`parse_test.go` pin this run's model name and answer text, so they will need the
new run's values; the token counts are asserted as non-zero rather than exactly,
so those survive a re-record.

## History

Recorded on 2.1.231 (2026-08-25), re-checked on 2.1.236, replacing a file whose
CLI version was never written down — the reason this one exists. That older
recording's `result` line lacked one key 2.1.231 emits,
`usage.output_tokens_details`, which the parser does not read; nothing the parser
does read has changed since.
