# Where these fixtures come from

All three files are recordings of real `codex exec --json` runs, verified
against codex-cli `0.163.0` with the argv the adapter itself builds:

```sh
codex exec --json --sandbox read-only --skip-git-repo-check --cd <scratch> \
           --ephemeral --ignore-rules --color never \
           -c features.shell_tool=false -c features.unified_exec=false \
           -c features.multi_agent=false -c features.goals=false \
           -c features.view_image=false -c features.plugins=false \
           -c features.apps=false -c features.computer_use=false \
           -c features.sleep_tool=false -c 'web_search="disabled"' \
           -c mcp_servers.<each enabled server>.enabled=false -
```

The prompt goes on stdin, which is what the trailing `-` selects. The `-c`
switches take the model's tools away; they arrived with the 0.163.0 re-check
(see History), after the files were recorded, and that re-check ran with them.
They change what the model is offered, not the shape of a run that uses no
tool, so the recordings stand.

- `simple.jsonl` — a turn with no reasoning item: one `agent_message` carrying
  the whole answer. This is the shape a plain completion takes.
- `reasoning.jsonl` — a turn with a `reasoning` item ahead of the message, and
  a non-zero `cached_input_tokens`. The cached count is the interesting part:
  codex reports cached tokens *inside* `input_tokens`, and the parser has to
  split them back out without losing the total.
- `quota-exceeded.jsonl` — a real failure: an account with no quota left.
  `turn.failed` carries the CLI's own explanation, and no `turn.completed`
  arrives, so the run must surface as an error rather than an empty answer.

Together they pin the event shapes the parser reads: `thread.started`,
`item.completed` for the `error` / `reasoning` / `agent_message` item types,
`turn.started`, `turn.completed` with `usage`, the top-level `error` line, and
`turn.failed` with `error.message`.

## The provider these were recorded against

The OpenAI account on the recording machine had no quota, so the successful runs
went through an OpenAI-compatible third-party endpoint configured as a
`model_providers` entry in `~/.codex/config.toml`. That choice is visible in the
recordings in two harmless ways, both of which a run against OpenAI would show
too:

- `input_tokens` is ~12.8k for a one-line prompt. That is codex's own agent
  prompt, not the caller's conversation.
- a `Model metadata for ... not found` warning item, which codex raises for any
  model it has no built-in metadata for.

The failure recording (`quota-exceeded.jsonl`) came from the OpenAI provider,
which is what had run out.

## The edits

The transcripts are byte-for-byte as recorded except for these, all textual:

1. `thread_id` was a real identifier from a real call. It is opaque and
   authenticates nothing, but there is no reason to publish it, so it reads
   `00000000-0000-7000-8000-000000000001` — the same shape, obviously synthetic.
2. The model name inside the `Model metadata for ...` warning named a private
   endpoint's deployment. It reads `` `fake-1` `` instead.

No event was added, removed or reordered, and no field was dropped: every edit
above replaces one string with another of the same shape.

The `` `[features].codex_hooks` is deprecated `` warning in the successful runs
is genuine — it comes from the recording machine's `config.toml`. It is kept
because a startup warning that must not fail the turn is exactly what
`TestWarningItemsAreNotFatal` covers.

## What the CLI does not emit

codex sends `item.completed` only. It has no `item.started` or `item.updated`,
and no partial-text events at all: an assistant message arrives whole or not at
all, which is why the codex backend cannot stream token by token the way
`claude --include-partial-messages` does.

`parser.Line` nevertheless routes all three `item.*` types to `parser.item`, so
two shapes it accepts have no recording behind them. They are covered by
constructed transcripts in `parse_test.go` — deliberately not in this directory,
which holds recordings only:

- `TestMessagesInOneTurnAreJoined` — more than one `agent_message` in a turn.
  A 0.148.0 recording used to show this; 0.149.1 did not reproduce it.
- `TestPartialItemTextWouldTruncate` — pins a **limitation, not a promise**. The
  dedup in `parser.item` is keyed by item id and the first non-empty text wins.
  That is right for an empty `item.started` followed by a full
  `item.completed`, and wrong if a future CLI ever puts partial text on
  `item.updated`: the fragment would win and the rest of the answer would be
  dropped, reported as a success. The test fails the day that changes.

## Tools

A run that uses a tool has no recording here either, on purpose. Making one
means running the CLI with its tools on, and the output carries whatever the
tool read off the recording machine. `TestToolItemStopsTheRun` replays the
order 0.163.0 produced instead — an announcement, the call, then an answer
built from its output — for each item type a run has shown
(`command_execution`, `mcp_tool_call`, `collab_tool_call`, `web_search`), the
two the CLI documents (`file_change`, `todo_list`), and one nobody has seen.

## Re-recording

An account with quota is all it takes: run the argv above in an empty directory,
scrub the two strings listed under "The edits", and replace the file. The
assertions in `parse_test.go` pin this run's text and token counts, so they will
need the new run's numbers.

`simple.jsonl` asserts its prose by first bytes, last bytes and length rather
than in full — a 632-byte paragraph inline would bury what the test is for.

## History

Recorded on 0.149.1, replacing files from 0.148.0; re-checked on 0.150.1,
0.151.0, 0.154.0, 0.155.1, 0.161.0 and 0.163.0. No shape has changed across
any of them.

Two things about that list are worth keeping.

0.152.0 was checked and deliberately **not** claimed: its flags were all still
accepted and its failure path still matched `quota-exceeded.jsonl`, but every
provider reachable from the recording machine was out of credit at the time, so
the successful shapes could not be observed. 0.154.0 finally could — the same
`item.completed`/`reasoning`, `item.completed`/`agent_message` and
`turn.completed` with the same five `usage` keys — which is what promotes the
header past 0.151.0.

The failure message has changed wording once, from `out of credits` on 0.148.0
to `Quota exceeded` since. It does not matter: `classify` matches on `quota` and
`billing`, not on the sentence.

One difference in the 0.154.0 run is not a shape change. It carried a single
`item.completed`/`error` warning where the recording has two, because the
recording machine's `config.toml` no longer sets the deprecated
`[features].codex_hooks`. The recording keeps both, since a startup warning that
must not fail the turn is what `TestWarningItemsAreNotFatal` covers.

0.161.0 was checked on both paths. A successful run through the third-party
provider matched `reasoning.jsonl` key for key, `usage` included. No provider
on the recording machine was out of quota this time, so the failure path was
observed through a different failure — the OpenAI provider refusing a model
name with a 404 — which produced the same lines `quota-exceeded.jsonl` pins:
top-level `error` per retry, an `item.completed`/`error`, and `turn.failed`
with no `turn.completed`. Its startup warning now reads `Codex is ignoring 2
unrecognized configuration settings`, naming two `model_providers.*` keys in
the recording machine's `config.toml`; a different sentence in the same
`item.completed`/`error` shape.

0.163.0 is the one that showed the model had tools all along. The re-check
used a prompt that invited working something out, and the model ran
`python3 -c` to do it: `item.started`/`item.completed` for a
`command_execution`, a type no recording here had shown. The read-only
sandbox blocks writes and the network, not reads, so a follow-up asking for
`ls -d "$HOME/.codex"` and the first line of `/etc/hosts` got both back.
0.161.0, still on the machine, did the same, so this was not a regression: the
earlier re-checks asked for a fixed line and never gave the model a reason to
reach for a tool.

The shell was the smallest part. Pointed at a stub provider, 0.163.0 sent a
third-party model fourteen tools under the old argv: the shell, `view_image`,
`request_user_input`, `spawn_agent` and four more agent tools, goals,
`web_search`, the MCP resource readers, and `node_repl` and `cua_repl` — MCP
servers from the recording machine's `config.toml` and a plugin. `node_repl`
ships with codex and holds the code it runs to the sandbox (a write beyond the
scratch directory came back `EPERM`), but a server the user adds does not: a
stub stdio server configured the same way wrote a file there as it started.
GPT-6 models get a different set in a different place (an `additional_tools`
input item, with a code-mode `exec` wrapping the rest). Provoked through the
third-party provider, `node_repl` produced an `mcp_tool_call`, `spawn_agent` a
`collab_tool_call`, `web_search` a `web_search` item; `create_goal` produced
no item at all.

The fix is two layers, the same lesson as Qwen Code 0.24.2. The argv now
switches off every feature that adds a tool (only those `codex features list`
names: an unknown one fails the run), disables web search, and disables each
MCP server `codex mcp list` reports enabled (`-c mcp_servers={}` merges rather
than clears). That leaves a third-party model `request_user_input` alone. The
guarantee is the parser: any item other than `agent_message`, `reasoning` or
`error` fails the run and stops the CLI, with a 502 or a stream error and none
of the tool's output. Both were checked end to end: the six provoking prompts
came back as plain answers ("NO SHELL" and so on), and with the shell switched
back on through `extra_args` the same request failed on `command_execution`
with no text sent.

The shapes themselves did not move. A successful run through the third-party
provider matched `reasoning.jsonl` key for key, `usage` included, with the new
argv. The failure path was observed as a quota failure again — one model's
quota ran out during the re-check — and matched `quota-exceeded.jsonl`:
top-level `error` per retry, `turn.failed`, no `turn.completed`.
