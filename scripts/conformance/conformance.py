#!/usr/bin/env python3
"""Drive agent2api with the real vendor SDKs and check what they see.

Unit tests can only assert what agent2api *writes*. What matters is what an
OpenAI or Anthropic SDK *does* with it: which exception type it raises, whether
its stream accumulator survives a keepalive, whether usage and stop reasons land
where its models expect them. That is what this suite checks, against the
scripted `mock` backend so every outcome — including a rate limit, a crash and a
timeout — is reproducible and free.

Run it through scripts/conformance/run.sh, which builds the gateway with the
right build tag and starts it with the matching config.
"""

from __future__ import annotations

import argparse
import sys
from typing import Any, Callable

import anthropic
import httpx
import openai

TIMEOUT = 60.0


class Suite:
    """A minimal test harness: one line per case, non-zero exit on failure."""

    def __init__(self, verbose: bool) -> None:
        self.verbose = verbose
        self.passed = 0
        self.failures: list[tuple[str, str]] = []

    def run(self, name: str, case: Callable[[], None]) -> None:
        try:
            case()
        except AssertionError as e:
            self.failures.append((name, str(e) or "assertion failed"))
            print(f"FAIL  {name}\n        {e}")
        except Exception as e:  # noqa: BLE001 - any failure is a failed case
            self.failures.append((name, f"{type(e).__name__}: {e}"))
            print(f"ERROR {name}\n        {type(e).__name__}: {e}")
        else:
            self.passed += 1
            print(f"ok    {name}")

    def report(self) -> int:
        print(f"\n{self.passed} passed, {len(self.failures)} failed")
        for name, why in self.failures:
            print(f"  - {name}: {why}")
        return 1 if self.failures else 0


def expect_error(exc: type[Exception], call: Callable[[], Any], *, status: int | None = None,
                 message: str | None = None, retry_after: str | None = None) -> Exception:
    """Assert that call raises exc, optionally checking status/message/header."""
    try:
        call()
    except exc as e:
        if status is not None:
            got = getattr(e, "status_code", None)
            assert got == status, f"status was {got}, want {status}"
        if message is not None:
            assert message in str(e), f"message {str(e)!r} does not mention {message!r}"
        if retry_after is not None:
            response = getattr(e, "response", None)
            got = response.headers.get("retry-after") if response is not None else None
            assert got == retry_after, f"Retry-After was {got!r}, want {retry_after!r}"
        return e
    raise AssertionError(f"expected {exc.__name__}, no exception was raised")


def openai_cases(suite: Suite, base_url: str, api_key: str) -> None:
    client = openai.OpenAI(base_url=f"{base_url}/v1", api_key=api_key, max_retries=0, timeout=TIMEOUT)
    run = lambda name, case: suite.run(f"openai: {name}", case)  # noqa: E731

    def models_listed() -> None:
        # One row per backend; a model is chosen by appending ":<model>".
        ids = {m.id for m in client.models.list().data}
        assert "mock" in ids, f"mock missing from {sorted(ids)}"

    def completion() -> None:
        r = client.chat.completions.create(
            model="mock:ok", messages=[{"role": "user", "content": "hi"}])
        assert r.object == "chat.completion", r.object
        assert r.model == "mock:ok", r.model
        assert r.id.startswith("chatcmpl-"), r.id
        choice = r.choices[0]
        assert choice.message.content == "Mock reply: ok.", repr(choice.message.content)
        assert choice.message.role == "assistant", choice.message.role
        assert choice.finish_reason == "stop", choice.finish_reason
        # 11 fresh + 3 cached input tokens, 5 out.
        assert (r.usage.prompt_tokens, r.usage.completion_tokens, r.usage.total_tokens) == (14, 5, 19), r.usage

    def empty_answer() -> None:
        r = client.chat.completions.create(
            model="mock:empty", messages=[{"role": "user", "content": "hi"}])
        assert r.choices[0].message.content == "", repr(r.choices[0].message.content)
        assert r.choices[0].finish_reason == "stop", r.choices[0].finish_reason

    def streaming() -> None:
        text, roles, finish, chunks = "", 0, None, 0
        for chunk in client.chat.completions.create(
                model="mock:ok", stream=True, messages=[{"role": "user", "content": "hi"}]):
            chunks += 1
            assert chunk.object == "chat.completion.chunk", chunk.object
            assert chunk.usage is None, "usage must be opt-in on a stream"
            for choice in chunk.choices:
                if choice.delta.role:
                    roles += 1
                text += choice.delta.content or ""
                finish = choice.finish_reason or finish
        assert chunks >= 3, f"only {chunks} chunks: the answer did not stream"
        assert roles == 1, f"the role was announced {roles} times, want exactly once"
        assert text == "Mock reply: ok.", repr(text)
        assert finish == "stop", finish

    def streaming_usage_opt_in() -> None:
        usage = None
        for chunk in client.chat.completions.create(
                model="mock:ok", stream=True, stream_options={"include_usage": True},
                messages=[{"role": "user", "content": "hi"}]):
            usage = chunk.usage or usage
        assert usage is not None, "no usage frame although include_usage was set"
        assert (usage.prompt_tokens, usage.completion_tokens) == (14, 5), usage

    def keepalive_survives() -> None:
        # The backend stays quiet for longer than the heartbeat interval, so the
        # stream carries keepalive comments the SDK's parser must ignore.
        text = "".join(
            (choice.delta.content or "")
            for chunk in client.chat.completions.create(
                model="mock:slow", stream=True, messages=[{"role": "user", "content": "hi"}])
            for choice in chunk.choices)
        assert text == "Mock reply: sorry for the wait.", repr(text)

    def reasoning_on_the_wire() -> None:
        # reasoning_content is not part of the OpenAI schema, so it is checked
        # on the wire rather than through the SDK's models.
        r = httpx.post(f"{base_url}/v1/chat/completions",
                       json={"model": "mock:thinking", "messages": [{"role": "user", "content": "hi"}]},
                       headers={"Authorization": f"Bearer {api_key}"}, timeout=TIMEOUT)
        r.raise_for_status()
        message = r.json()["choices"][0]["message"]
        assert message["reasoning_content"] == "Considering the question carefully.", message
        assert message["content"] == "Mock reply: thought about it.", message

    def request_id_header() -> None:
        raw = client.chat.completions.with_raw_response.create(
            model="mock:ok", messages=[{"role": "user", "content": "hi"}])
        assert raw.request_id, "the SDK found no request id on the response"
        assert raw.headers.get("request-id") == raw.request_id, raw.headers.get("request-id")

    def unknown_model() -> None:
        expect_error(openai.NotFoundError, lambda: client.chat.completions.create(
            model="nope", messages=[{"role": "user", "content": "hi"}]),
            status=404, message="not available")

    def tools_rejected() -> None:
        expect_error(openai.BadRequestError, lambda: client.chat.completions.create(
            model="mock:ok", messages=[{"role": "user", "content": "hi"}],
            tools=[{"type": "function", "function": {"name": "x"}}]),
            status=400, message="tool calling")

    def bad_key() -> None:
        wrong = openai.OpenAI(base_url=f"{base_url}/v1", api_key="sk-wrong", max_retries=0, timeout=TIMEOUT)
        expect_error(openai.AuthenticationError, lambda: wrong.chat.completions.create(
            model="mock:ok", messages=[{"role": "user", "content": "hi"}]), status=401)

    def upstream_errors() -> None:
        cases = [
            ("mock:unauthenticated", openai.APIStatusError, 503, "not logged in"),
            ("mock:crash", openai.APIStatusError, 502, "failed"),
            ("mock:timeout", openai.APIStatusError, 504, "idle_timeout"),
            ("mock:truncated", openai.APIStatusError, 502, "without completing"),
        ]
        for model, exc, status, message in cases:
            expect_error(exc, lambda m=model: client.chat.completions.create(
                model=m, messages=[{"role": "user", "content": "hi"}]),
                status=status, message=message)

    def rate_limit_carries_retry_after() -> None:
        expect_error(openai.RateLimitError, lambda: client.chat.completions.create(
            model="mock:rate-limited", messages=[{"role": "user", "content": "hi"}]),
            status=429, message="rate limited", retry_after="10")

    def failure_before_the_stream_starts() -> None:
        # Nothing has been written yet, so the SDK must see a real status code
        # rather than an error inside a 200 OK stream.
        expect_error(openai.APIStatusError, lambda: list(client.chat.completions.create(
            model="mock:crash", stream=True, messages=[{"role": "user", "content": "hi"}])),
            status=502)

    def failure_inside_the_stream() -> None:
        text = ""
        def drain() -> None:
            nonlocal text
            for chunk in client.chat.completions.create(
                    model="mock:mid-stream-failure", stream=True,
                    messages=[{"role": "user", "content": "hi"}]):
                for choice in chunk.choices:
                    text += choice.delta.content or ""
        expect_error(openai.APIError, drain, message="halfway")
        assert text.startswith("Mock reply:"), f"the partial answer was lost: {text!r}"

    for name, case in [
        ("models are advertised", models_listed),
        ("completion", completion),
        ("empty answer", empty_answer),
        ("streaming", streaming),
        ("streaming usage is opt-in", streaming_usage_opt_in),
        ("keepalives do not break the stream", keepalive_survives),
        ("reasoning_content on the wire", reasoning_on_the_wire),
        ("request id header", request_id_header),
        ("unknown model is 404", unknown_model),
        ("tools are refused", tools_rejected),
        ("bad key is 401", bad_key),
        ("upstream failures keep their status", upstream_errors),
        ("rate limit carries Retry-After", rate_limit_carries_retry_after),
        ("failure before the stream starts is a status", failure_before_the_stream_starts),
        ("failure inside the stream is raised", failure_inside_the_stream),
    ]:
        run(name, case)


def anthropic_cases(suite: Suite, base_url: str, api_key: str) -> None:
    client = anthropic.Anthropic(base_url=base_url, api_key=api_key, max_retries=0, timeout=TIMEOUT)
    run = lambda name, case: suite.run(f"anthropic: {name}", case)  # noqa: E731

    def message() -> None:
        m = client.messages.create(model="mock:ok", max_tokens=64,
                                   messages=[{"role": "user", "content": "hi"}])
        assert m.type == "message" and m.role == "assistant", m
        assert m.model == "mock:ok", m.model
        assert m.id.startswith("msg_"), m.id
        assert [b.type for b in m.content] == ["text"], [b.type for b in m.content]
        assert m.content[0].text == "Mock reply: ok.", repr(m.content[0].text)
        assert m.stop_reason == "end_turn", m.stop_reason
        assert (m.usage.input_tokens, m.usage.output_tokens) == (11, 5), m.usage
        assert m.usage.cache_read_input_tokens == 3, m.usage

    def thinking_block() -> None:
        m = client.messages.create(model="mock:thinking", max_tokens=64,
                                   messages=[{"role": "user", "content": "hi"}])
        assert [b.type for b in m.content] == ["thinking", "text"], [b.type for b in m.content]
        assert m.content[0].thinking == "Considering the question carefully.", m.content[0]
        assert m.content[0].signature == "", "a thinking block needs a signature field"
        assert m.content[1].text == "Mock reply: thought about it.", m.content[1]

    def empty_answer() -> None:
        m = client.messages.create(model="mock:empty", max_tokens=8,
                                   messages=[{"role": "user", "content": "hi"}])
        assert [b.type for b in m.content] == ["text"], [b.type for b in m.content]
        assert m.content[0].text == "", repr(m.content[0].text)

    def system_prompt() -> None:
        m = client.messages.create(model="mock:ok", max_tokens=8, system="Be terse.",
                                   messages=[{"role": "user", "content": "hi"}])
        assert m.content[0].text == "Mock reply: ok.", m.content[0]

    def streaming() -> None:
        with client.messages.stream(model="mock:ok", max_tokens=64,
                                    messages=[{"role": "user", "content": "hi"}]) as stream:
            text = "".join(stream.text_stream)
            final = stream.get_final_message()
        assert text == "Mock reply: ok.", repr(text)
        assert final.stop_reason == "end_turn", final.stop_reason
        assert (final.usage.input_tokens, final.usage.output_tokens) == (11, 5), final.usage

    def streaming_thinking() -> None:
        with client.messages.stream(model="mock:thinking", max_tokens=64,
                                    messages=[{"role": "user", "content": "hi"}]) as stream:
            final = stream.get_final_message()
        assert [b.type for b in final.content] == ["thinking", "text"], [b.type for b in final.content]
        assert final.content[0].thinking == "Considering the question carefully.", final.content[0]

    def pings_survive() -> None:
        # The accumulator rejects any event before message_start, so a ping sent
        # while the backend is quiet must not be the first thing on the wire.
        with client.messages.stream(model="mock:slow", max_tokens=64,
                                    messages=[{"role": "user", "content": "hi"}]) as stream:
            text = "".join(stream.text_stream)
        assert text == "Mock reply: sorry for the wait.", repr(text)

    def request_id_header() -> None:
        raw = client.messages.with_raw_response.create(
            model="mock:ok", max_tokens=8, messages=[{"role": "user", "content": "hi"}])
        assert raw.request_id, "the SDK found no request id on the response"

    def unknown_model() -> None:
        expect_error(anthropic.NotFoundError, lambda: client.messages.create(
            model="nope", max_tokens=8, messages=[{"role": "user", "content": "hi"}]),
            status=404, message="not available")

    def tools_rejected() -> None:
        expect_error(anthropic.BadRequestError, lambda: client.messages.create(
            model="mock:ok", max_tokens=8, messages=[{"role": "user", "content": "hi"}],
            tools=[{"name": "x", "description": "y", "input_schema": {"type": "object"}}]),
            status=400, message="tool calling")

    def stop_sequences_rejected() -> None:
        expect_error(anthropic.BadRequestError, lambda: client.messages.create(
            model="mock:ok", max_tokens=8, messages=[{"role": "user", "content": "hi"}],
            stop_sequences=["END"]), status=400, message="stop sequences")

    def bad_key() -> None:
        wrong = anthropic.Anthropic(base_url=base_url, api_key="sk-wrong", max_retries=0, timeout=TIMEOUT)
        expect_error(anthropic.AuthenticationError, lambda: wrong.messages.create(
            model="mock:ok", max_tokens=8, messages=[{"role": "user", "content": "hi"}]), status=401)

    def upstream_errors() -> None:
        cases = [
            ("mock:unauthenticated", 503, "not logged in"),
            ("mock:crash", 502, "failed"),
            ("mock:timeout", 504, "idle_timeout"),
            ("mock:truncated", 502, "without completing"),
        ]
        for model, status, message in cases:
            expect_error(anthropic.APIStatusError, lambda m=model: client.messages.create(
                model=m, max_tokens=8, messages=[{"role": "user", "content": "hi"}]),
                status=status, message=message)

    def rate_limit_carries_retry_after() -> None:
        expect_error(anthropic.RateLimitError, lambda: client.messages.create(
            model="mock:rate-limited", max_tokens=8, messages=[{"role": "user", "content": "hi"}]),
            status=429, message="rate limited", retry_after="10")

    def failure_before_the_stream_starts() -> None:
        def drain() -> None:
            with client.messages.stream(model="mock:crash", max_tokens=8,
                                        messages=[{"role": "user", "content": "hi"}]) as stream:
                list(stream.text_stream)
        expect_error(anthropic.APIStatusError, drain, status=502)

    def failure_inside_the_stream() -> None:
        text = ""
        def drain() -> None:
            nonlocal text
            with client.messages.stream(model="mock:mid-stream-failure", max_tokens=64,
                                        messages=[{"role": "user", "content": "hi"}]) as stream:
                for part in stream.text_stream:
                    text += part
        expect_error(anthropic.APIStatusError, drain, message="halfway")
        assert text.startswith("Mock reply:"), f"the partial answer was lost: {text!r}"

    for name, case in [
        ("message", message),
        ("thinking block", thinking_block),
        ("empty answer", empty_answer),
        ("system prompt", system_prompt),
        ("streaming", streaming),
        ("streaming thinking block", streaming_thinking),
        ("pings do not break the accumulator", pings_survive),
        ("request id header", request_id_header),
        ("unknown model is 404", unknown_model),
        ("tools are refused", tools_rejected),
        ("stop sequences are refused", stop_sequences_rejected),
        ("bad key is 401", bad_key),
        ("upstream failures keep their status", upstream_errors),
        ("rate limit carries Retry-After", rate_limit_carries_retry_after),
        ("failure before the stream starts is a status", failure_before_the_stream_starts),
        ("failure inside the stream is raised", failure_inside_the_stream),
    ]:
        run(name, case)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8799")
    parser.add_argument("--api-key", default="sk-conformance")
    parser.add_argument("-v", "--verbose", action="store_true")
    args = parser.parse_args()

    print(f"agent2api conformance: {args.base_url}")
    print(f"openai {openai.__version__} · anthropic {anthropic.__version__}\n")

    suite = Suite(args.verbose)
    openai_cases(suite, args.base_url, args.api_key)
    print()
    anthropic_cases(suite, args.base_url, args.api_key)
    return suite.report()


if __name__ == "__main__":
    sys.exit(main())
