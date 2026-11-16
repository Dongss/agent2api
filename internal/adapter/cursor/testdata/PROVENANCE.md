# Where these fixtures come from

Both files are recordings of real `cursor-agent` runs, recorded on version
`2026.11.13-eacade1`, captured with the argv the adapter itself builds:

```sh
cursor-agent --print --output-format stream-json [--stream-partial-output] \
             --mode ask --sandbox enabled --trust "<prompt>"
```

in a scratch directory holding the `.cursor/cli.json` that `writePermissions`
generates:

```json
{"permissions":{"allow":[],"deny":["Read(**)","Write(**)","Shell(*)","WebFetch(*)","Mcp(*:*)"]}}
```

The prompts were `Reply with exactly: Hello from the fixture.` (`partial.jsonl`)
and `Reply with exactly: Same chunk.` (`chunked.jsonl`).

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
`cacheReadTokens` / `cacheWriteTokens` / `reasoningTokens`.

## The edits

The transcripts are byte-for-byte as recorded except for these, all textual:

1. A nickname the account's global Cursor rules made the model mention in its
   reasoning was replaced with `REDACTED` — once in each file. It is someone's
   name, and these files are committed. The same rule sometimes puts the
   nickname in the answer itself; a run that did was recorded again rather than
   scrubbed, so the asserted answers are the prompts' own lines.
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

## Tools

A run that uses a tool has no recording here, on purpose: making one means
letting the CLI use its tools, and the output carries whatever they read off
the recording machine. `TestToolCallStopsTheRun` replays the order
`2026.11.13-eacade1` produced instead — the announcement streamed token by
token, its flush, `tool_call` started and completed, then an answer built from
the tool's output — for each kind of call those runs showed and one nobody has
seen. The kind is the `...ToolCall` key inside `tool_call`; see History for how
they turned up.

## Re-recording

An account is all it takes: run the argv above in an empty directory holding
the `cli.json` above, scrub the three things listed under "The edits", and
replace the file. A run whose answer carries the nickname is recorded again,
not scrubbed. The assertions in `parse_test.go` pin this run's model name,
thinking text and token counts, so they will need the new run's values.

## History

First recorded on `2026.08.11-e8db854`, re-checked on `2026.08.25-3e8eec8`,
`2026.08.31-4057e58`, `2026.09.08-6caf4ff`, `2026.09.15-d2fe57e`,
`2026.09.18-9a7762b`, `2026.10.01-e373342` and `2026.10.16-e3cb207`: no event
type, field or `usage` key changed across any of them, with or without
`--stream-partial-output`. None
of them has a flag for disabling session persistence, so a run still leaves
state under `~/.cursor`; the `persist` command `2026.10.01-e373342` added goes
the other way, keeping a session alive across disconnects.

`2026.11.13-eacade1` (2026-11-16) is the first change, and it is only a field:
`result.usage` gained `reasoningTokens`, the part of `outputTokens` spent
thinking (104 of 109 in `partial.jsonl`). The parser now reads it as
`ReasoningOutputTokens`, which is why both files were recorded again rather
than re-checked: the old ones could not show it. Nothing else about either
shape moved, and a recording made without the permissions file had the same
shape as one made with it.

The same re-check is where the tools turned up. Earlier re-checks asked for a
fixed line and never gave the model a reason to reach for one; asked to list
its directory and print the first lines of `/etc/hosts`, the model in ask mode
tried the shell (refused: ask mode is read-only), then read `/etc/hosts` with
`readToolCall` and put the lines in the answer. Ask mode is read-only, not
tool-free. Other prompts brought out `globToolCall`, `grepToolCall` (which
found `# Host Database`), `updateTodosToolCall`, `getMcpToolsToolCall` — the
CLI's own `cursor` server, offering `Task`, `WebSearch` and `TodoWrite` — and
`taskToolCall`, which started a sub-agent. Asked to list `/etc`, the sub-agent
answered twice with names this machine's `/etc` does not hold, so where it
ran is not known. `webFetchToolCall` came back `rejected`. Self-contained prompts (code to write, a mutex to explain, a
sentence to translate) used no tool in six runs out of six; prompts about "my
project" or "this repository" used up to fourteen.

No flag removes them, so the fix is two layers, as for codex and Qwen Code. The
scratch directory gets the `cli.json` above: `Read(**)` turned the read into
`Permission denied` and the grep into no matches, while the other entries
changed nothing measurable (the shell and the fetch were refused without them
too). `Task` and `getMcpTools` ran with it in place. The guarantee is the
parser: any `tool_call` line fails the run with a 502 naming its kind and stops
the CLI, so the tool's output never reaches the caller. Checked end to end
through the gateway, streamed and not: the provoking prompts came back as that
502, and the six self-contained ones still answered.
