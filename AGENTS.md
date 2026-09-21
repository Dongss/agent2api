# Working on agent2api

agent2api exposes the agent CLIs installed on a machine (Claude Code, Codex,
Cursor, Qwen Code) as OpenAI- and Anthropic-compatible HTTP APIs. It drives
them as plain LLMs: tools off, empty scratch workdir, allowlisted environment.

```sh
go test ./...              # no CLI or account required
go test -race ./...
scripts/conformance/run.sh # drives both APIs with the real vendor SDKs
```

## Layout

```
cmd/agent2api/      serve, doctor, config print, update, version
internal/frontend/  openai/, responses/, anthropic/, sse/   — wire formats
internal/ir/        the protocol-neutral middle: Request, Event, Error
internal/adapter/   claudecode/, codex/, cursor/, qwencode/ — argv + output parsing
internal/adapter/agentcli/  what they share: probing, run loop, redaction
internal/runner/    subprocess lifecycle, deadlines, process trees
internal/config/    schema, Go defaults, strict decode, flags
internal/selfupdate/  `update`: fetch, verify and swap in a release binary
```

No frontend imports a backend and no backend imports a frontend; everything
meets at `ir`.

## Invariants worth knowing before you change something

- **Stateless.** Every request replays the whole conversation into a fresh CLI
  process. No sessions, no cross-request state. This is what makes history
  edits, retries and concurrent clients correct. The Responses frontend refuses
  `previous_response_id` for this reason rather than approximating it, and
  reports `"store": false` instead of quietly not storing.
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
- **A capability is per-installed-CLI, not per-adapter.** Structured output was
  the first and reasoning effort the second: `adapter.SchemaEnforcer` and
  `adapter.EffortSetter` are optional interfaces, answered by probing the
  binary's flags, and a frontend asks before accepting the parameter so the
  refusal names the backend. Where the vocabularies disagree the gap stays
  visible — `EffortSetter` reports the levels it has rather than a yes/no, so
  a level a backend lacks is refused by name instead of rounded to a
  neighbour.
  Anything wrapping an adapter must forward such an interface explicitly —
  embedding `adapter.Adapter` promotes only the methods that interface
  declares, so `gate` silently dropped it until told not to.
- **`update` verifies or refuses.** The install scripts warn and continue when
  `checksums.txt` is missing — reasonable when creating a file that did not
  exist. `update` overwrites a binary the user already trusts, so an absent or
  mismatched checksum is fatal. Asset names must stay in step with
  `scripts/build.sh` and both installers.
- **The golden transcripts are recordings.** Fixtures under
  `internal/adapter/*/testdata/` came from real CLI runs, so a CLI upgrade that
  changes event shapes fails the tests instead of corrupting answers. Re-record
  rather than edit to fit, and record which CLI version a fixture came from:
  every `testdata/` directory carries a `PROVENANCE.md` with the argv used, the
  scrubbing applied, and how to re-record. A shape the CLI does not emit belongs
  in a constructed transcript in the test file, not in `testdata/`.

## Conventions

- Comments say *why*, not what. The reader can see what the code does.
- An error a user can act on names the thing to change: the config key, the
  command to run, the value to write.
- Refuse what cannot be served honestly (client tools, `n>1`, stop sequences)
  with a 400 that explains itself, rather than silently ignoring it.
