# Where these fixtures come from

Both files are recordings of real `cursor-agent` runs, verified against version
`2026.08.25-3e8eec8`, captured with the argv the adapter itself builds:

```sh
cursor-agent --print --output-format stream-json [--stream-partial-output] \
             --mode ask --sandbox enabled --trust "<prompt>"
```

- `partial.jsonl` — with `--stream-partial-output`, which is what the adapter
  passes when the installed CLI supports it. One `assistant` line per token,
  then **a flush line repeating the whole answer** right before `result`. That
  repeat is the thing the parser exists to drop; without it the caller would
  receive every answer twice.
- `chunked.jsonl` — without the flag. The whole answer arrives as a single
  `assistant` line, which *is* the flush, so there is nothing to deduplicate —
  the opposite case, and the one that catches a parser too eager to treat a
  repeat as a duplicate.

Together they pin the event shapes the parser reads: `system`/`init` (model
name), the `user` echo line, `thinking`/`delta` and `thinking`/`completed`, the
`assistant` content array, and `result` with `inputTokens` / `outputTokens` /
`cacheReadTokens` / `cacheWriteTokens`.

## The edits

The transcripts are byte-for-byte as recorded except for these, all textual:

1. A nickname the account's global Cursor rules made the model mention in its
   reasoning was replaced with `REDACTED` — once in `partial.jsonl`, twice in
   `chunked.jsonl`. It is someone's name, and these files are committed.
2. `system`/`init` reports its `cwd`, and the recording was made by hand rather
   than through the gateway, so the path was an ordinary scratch directory of
   the moment. It now reads `/tmp/agent2api-cursor-2f8c1d`, which is the shape
   the gateway actually produces.
3. `session_id` and `request_id` were real identifiers from a real account's
   call. They are opaque and authenticate nothing, but there is no reason to
   publish them, so they read `00000000-0000-4000-8000-00000000000N` — the same
   shape, obviously synthetic. The other fixtures were scrubbed the same way.

No event was added, removed or reordered, and no field was dropped: every edit
above replaces one string with another of the same shape.

## Re-recording

An account is all it takes: run the argv above in an empty directory and replace
the file. The assertions in `parse_test.go` pin this run's model name and token
counts, so they will need the new run's numbers.

## History

Recorded on `2026.08.11-e8db854`, re-checked on `2026.08.25-3e8eec8`: no event
type, field or `usage` key changed. Neither version has a flag for disabling
session persistence, so a run still leaves state under `~/.cursor`.
