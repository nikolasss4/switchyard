"""The official OpenAI Python SDK against the gateway's OpenAI-compatible surface.
`fast` is served by an OpenAI-compatible upstream, `smart` by an Anthropic one,
so every call also exercises format translation in one of the directions."""

import json

import openai
import pytest

from conftest import WEATHER_OPENAI

MODELS = ["fast", "smart"]


@pytest.mark.parametrize("model", MODELS)
def test_chat_completion(oai, model):
    r = oai.chat.completions.with_raw_response.create(
        model=model,
        messages=[
            {"role": "system", "content": "be brief"},
            {"role": "user", "content": "say hello"},
        ],
        max_tokens=12,
    )
    c = r.parse()
    # `fast` has the exact cache on, so a rerun may be served from it.
    assert r.headers.get("x-switchyard-provider") or r.headers.get("x-switchyard-cache") == "exact"
    assert c.object == "chat.completion"
    assert c.choices[0].message.role == "assistant"
    assert c.choices[0].message.content
    assert c.choices[0].finish_reason == "length"
    assert c.usage.completion_tokens == 12 and c.usage.total_tokens == c.usage.prompt_tokens + 12


@pytest.mark.parametrize("model", MODELS)
def test_streaming_with_usage(oai, model):
    stream = oai.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "count"}],
        stream=True,
        stream_options={"include_usage": True},
    )
    text, finish, usage = "", None, None
    for chunk in stream:
        if chunk.usage:
            usage = chunk.usage
        for ch in chunk.choices:
            text += ch.delta.content or ""
            finish = ch.finish_reason or finish
    assert len(text.split()) == 32
    assert finish == "stop"
    assert usage and usage.completion_tokens == 32


@pytest.mark.parametrize("model", MODELS)
def test_tool_call_round_trip(oai, model):
    messages = [{"role": "user", "content": "weather in Paris?"}]
    first = oai.chat.completions.create(model=model, messages=messages, tools=[WEATHER_OPENAI])
    call = first.choices[0].message.tool_calls[0]
    assert first.choices[0].finish_reason == "tool_calls"
    assert call.function.name == "get_weather"
    assert json.loads(call.function.arguments) == {"city": "mock"}

    messages += [
        first.choices[0].message.model_dump(exclude_none=True),
        {"role": "tool", "tool_call_id": call.id, "content": "sunny, 21C"},
    ]
    second = oai.chat.completions.create(model=model, messages=messages, tools=[WEATHER_OPENAI])
    assert second.choices[0].finish_reason == "stop"
    assert second.choices[0].message.content


@pytest.mark.parametrize("model", MODELS)
def test_streamed_tool_call(oai, model):
    stream = oai.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "weather?"}],
        tools=[WEATHER_OPENAI],
        stream=True,
    )
    name, args, finish = "", "", None
    for chunk in stream:
        for ch in chunk.choices:
            for tc in ch.delta.tool_calls or []:
                assert tc.index == 0
                name += tc.function.name or ""
                args += tc.function.arguments or ""
            finish = ch.finish_reason or finish
    assert name == "get_weather" and json.loads(args) == {"city": "mock"} and finish == "tool_calls"


def test_embeddings(oai):
    r = oai.embeddings.create(model="embed", input=["switchyard", "a gateway"])
    assert len(r.data) == 2 and len(r.data[0].embedding) == 256 and r.model == "embed"


def test_models_list(oai):
    ids = {m.id for m in oai.models.list()}
    assert {"fast", "smart"} <= ids


def test_errors(oai, make_key):
    with pytest.raises(openai.AuthenticationError):
        openai.OpenAI(
            base_url=str(oai.base_url), api_key="sy_nope0000_" + "x" * 32, max_retries=0
        ).chat.completions.create(model="fast", messages=[{"role": "user", "content": "x"}])
    with pytest.raises(openai.NotFoundError):
        oai.chat.completions.create(model="no-such-model", messages=[{"role": "user", "content": "x"}])
    with pytest.raises(openai.BadRequestError):
        oai.chat.completions.create(model="fast", messages=[{"role": "robot", "content": "x"}])

    limited = openai.OpenAI(base_url=str(oai.base_url), api_key=make_key(rpm=1), max_retries=0)
    limited.chat.completions.create(model="fast", messages=[{"role": "user", "content": "x"}])
    with pytest.raises(openai.RateLimitError) as e:
        limited.chat.completions.create(model="fast", messages=[{"role": "user", "content": "x"}])
    assert int(e.value.response.headers["retry-after"]) > 0

    broke = openai.OpenAI(
        base_url=str(oai.base_url),
        api_key=make_key(monthly_budget_usd=0),
        max_retries=0,
    )
    with pytest.raises(openai.APIStatusError) as e:
        broke.chat.completions.create(model="fast", messages=[{"role": "user", "content": "x"}])
    assert e.value.status_code == 402
