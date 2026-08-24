# Working on agent2api

agent2api exposes the agent CLIs installed on a machine (Claude Code, Codex,
Cursor) as OpenAI- and Anthropic-compatible HTTP APIs. It drives them as plain
LLMs: tools off, empty scratch workdir, allowlisted environment.

```sh
go test ./...              # no CLI or account required
go test -race ./...
scripts/conformance/run.sh # drives both APIs with the real vendor SDKs
```

## Layout

```
cmd/agent2api/      serve, doctor, config print, version
internal/frontend/  openai/, anthropic/, sse/   — wire formats
internal/ir/        the protocol-neutral middle: Request, Event, Error
internal/adapter/   claudecode/, codex/, cursor/ — argv + output parsing
internal/adapter/agentcli/  what those three share: probing, run loop, redaction
internal/runner/    subprocess lifecycle, deadlines, process trees
internal/config/    schema, Go defaults, strict decode, flags
```

No frontend imports a backend and no backend imports a frontend; everything
meets at `ir`.

## Invariants worth knowing before you change something

- **Stateless.** Every request replays the whole conversation into a fresh CLI
  process. No sessions, no cross-request state. This is what makes history
  edits, retries and concurrent clients correct.
- **Adapters always stream.** Non-streaming responses are the same code path,
  collected to the end. Do not add a second one.
- **A truncated answer is an error, not a short answer.** A run that ends
  without a terminal event must fail loudly; the caller cannot tell otherwise.
- **Caller content never reaches the log**, at any level. Prompts go on stdin
  where the CLI allows it; what has to go in argv is redacted before logging.
- **Unknown config keys are a startup error.** The schema is `internal/config`;
  defaults live in Go (`defaults.go`), and `agent2api.example.yaml` documents
  every key — a test fails if the two drift.
- **`config print` and the logs redact anything that can hold a credential.**
- **The golden transcripts are recordings.** Fixtures under
  `internal/adapter/*/testdata/` came from real CLI runs, so a CLI upgrade that
  changes event shapes fails the tests instead of corrupting answers. Re-record
  rather than edit to fit; see `cursor/testdata/PROVENANCE.md` for the form.

## Conventions

- Comments say *why*, not what. The reader can see what the code does.
- An error a user can act on names the thing to change: the config key, the
  command to run, the value to write.
- Refuse what cannot be served honestly (client tools, `n>1`, stop sequences)
  with a 400 that explains itself, rather than silently ignoring it.
