# agent2api

[![license](https://img.shields.io/github/license/Dongss/agent2api)](LICENSE)
[![version](https://img.shields.io/github/v/release/Dongss/agent2api?include_prereleases&label=version)](https://github.com/Dongss/agent2api/releases/latest)
[![CI](https://github.com/Dongss/agent2api/actions/workflows/ci.yml/badge.svg)](https://github.com/Dongss/agent2api/actions/workflows/ci.yml)
![coverage](https://img.shields.io/endpoint?url=https://gist.githubusercontent.com/Dongss/fb5b6041e3db32f9765ecaba9969e868/raw/agent2api-coverage.json)

Expose local agent CLIs (Claude Code, Cursor, Codex, and more) as OpenAI- and Anthropic-compatible LLM APIs over HTTP.

**agent2api** turns the agent CLIs already installed on your machine into standard LLM HTTP APIs. Point any OpenAI or Anthropic SDK at a local endpoint, and requests are translated to the underlying CLI transparently — no extra API keys, no vendor lock-in, no client-side changes.

## Supported agents

- [x] Claude Code (`claude`)
- [x] Codex (`codex exec`)
- [x] Cursor (`cursor-agent`)

## Installation

macOS / Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/Dongss/agent2api/main/scripts/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/Dongss/agent2api/main/scripts/install.ps1 | iex
```

**Windows is experimental.** 

## Quick start

```sh
agent2api           # print help: the bare command never starts a server
agent2api doctor    # check which CLIs are installed, on PATH and logged in
agent2api serve     # serve on 127.0.0.1:8055
```

```sh
curl http://127.0.0.1:8055/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-code:opus",
    "messages": [{"role": "user", "content": "Write a haiku about subprocesses."}]
  }'
```

Any OpenAI SDK works by overriding the base URL:

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8055/v1", api_key="unused")
stream = client.chat.completions.create(
    model="claude-code:opus",
    messages=[{"role": "user", "content": "Hello"}],
    stream=True,
)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="")
```

So does any Anthropic SDK, against the same gateway:

```python
from anthropic import Anthropic

client = Anthropic(base_url="http://127.0.0.1:8055", api_key="unused")
with client.messages.stream(
    model="claude-code:opus",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello"}],
) as stream:
    for text in stream.text_stream:
        print(text, end="")
```

## Model names

Models are namespaced `<adapter>[:<model>]`:

| Name | Resolves to |
|---|---|
| `claude-code` | whichever model the `claude` CLI picks on its own |
| `claude-code:opus`, `codex:gpt-5.6-sol`, `cursor:composer` | that model |

Everything after the colon reaches the CLI verbatim, so there is no model list to
go stale: a name your account just gained works immediately, and a bad one is
rejected by the CLI. An unknown adapter is a `404`.
`GET /v1/models` lists one row per backend.

## Commands

```
agent2api                      # print help; the bare command starts nothing
agent2api serve [flags]        # start the gateway
agent2api doctor [flags]       # probe every configured adapter, print a table
agent2api config print [flags] # print the effective merged config and exit
agent2api version
```

`doctor` checks the binary, the flags the installed version supports, and login
state — the same probe `serve` runs to decide what to offer. It sends no prompt,
so it costs nothing:

```
ADAPTER      STATUS       BINARY                              VERSION                ACCOUNT          MODELS
claude-code  ok           /opt/homebrew/bin/claude            2.1.228 (Claude Code)  you@example.com  claude-code, claude-code:<model>
codex        ok           /Users/you/.local/bin/codex         codex-cli 0.148.0      ChatGPT          codex, codex:<model>
cursor       unavailable  /Users/you/.local/bin/cursor-agent  2026.08.11-e8db854     -                cursor, cursor:<model>

cursor: the Cursor Agent CLI is not logged in; run `cursor-agent login` (Not logged in)
```

Backends marked `unavailable` are simply skipped at startup; `doctor` exits
non-zero only if nothing at all is usable.

## Configuration

Pass a config file with `-c/--config` — see
[`agent2api.example.yaml`](agent2api.example.yaml) for every key; without one the
built-in defaults apply, and `agent2api config print` shows what is in effect.

Flags cover the settings worth changing per run and no more: `--config`,
`--host`, `--port`, `--api-key`, `--log-level`, `--log-file`,
`--max-concurrency`.

```sh
agent2api serve --port 9000
agent2api serve -c ./agent2api.yaml --log-level debug
```

### Auth

Binds to `127.0.0.1` with no authentication by default. Setting `api_key` (or
`--api-key`) requires every request to present it, as either
`Authorization: Bearer <key>` or `x-api-key: <key>`. Binding to a non-loopback
address without a key is refused at startup.

agent2api never touches vendor credentials: each CLI uses its own login.

## What is not supported

These return a clear `400` rather than a wrong answer:

- client `tools` / `tool_choice`, `n > 1`, `logprobs`, image and document input.
- `stop` / `stop_sequences`: no agent CLI can enforce them, and returning text
  the caller asked to have cut would be worse than refusing.
- A conversation over 96 KiB on the `cursor` backend: that CLI takes its prompt
  as a command-line argument, which the OS bounds.

`temperature` and `max_tokens` are accepted and passed along as best effort; none
of the CLIs has a flag for either, so they do not take effect today. Anthropic's
`max_tokens` is optional here for the same reason, though the vendor API requires
it. Thinking output is surfaced where the dialect has a place for it: `thinking`
content blocks for Anthropic, the non-standard `reasoning_content` field for
OpenAI.

## Development

```sh
go test ./...          # unit tests, no CLI required
go test -race ./...
```

Each adapter's parser is covered by a golden transcript, so a CLI upgrade that
changes the event shapes fails the tests instead of quietly corrupting
responses:

- `internal/adapter/claudecode/testdata/` — recorded from a real `claude` run.
- `internal/adapter/codex/testdata/` — recorded from real `codex exec --json`
  runs, including a failing one.
- `internal/adapter/cursor/testdata/` — recorded from real `cursor-agent` runs,
  one per streaming shape the CLI has. See that directory's `PROVENANCE.md` for
  the edits it carries.

Session and request identifiers in all three are synthetic: the recordings are
real, but there is no reason to publish the ids of someone's actual calls.

### Conformance suite

Unit tests can only assert what agent2api *writes*. What matters is what a real
SDK *does* with it — which exception type it raises, whether its stream
accumulator survives a keepalive, whether usage lands where its models expect
it. `scripts/conformance/` drives both surfaces with the real `openai` and
`anthropic` Python SDKs and checks exactly that:

```sh
scripts/conformance/run.sh
```

It builds the gateway with `-tags conformance`, which links in a scripted `mock`
backend, and starts it with `scripts/conformance/config.yaml`. The mock is how
outcomes nobody can summon on demand — a rate limit, a crash, a timeout, a CLI
that dies mid-answer — become reproducible and free: no CLI, no account, no
tokens. The suite covers both dialects streaming and not, every error class with
its status and SDK exception type, `Retry-After`, request ids, and thinking
output.

The mock backend is deliberately absent from a normal build: a gateway that
ships a backend which fabricates answers is a gateway that can lie to its user.

## Disclaimer

agent2api is an adapter/interface layer and does not include or redistribute third-party agent CLI software. Each agent CLI must be installed and authenticated separately, and its use is subject to the respective vendor's terms of service. All product names and trademarks are the property of their respective owners.

## License

[Apache License 2.0](LICENSE)
