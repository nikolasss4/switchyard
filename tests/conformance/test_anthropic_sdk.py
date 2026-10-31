"""The official Anthropic Python SDK against the gateway's /v1/messages surface."""

import anthropic
import pytest

from conftest import WEATHER_ANTHROPIC

MODELS = ["fast", "smart"]


@pytest.mark.parametrize("model", MODELS)
def test_message(anth, model):
    raw = anth.messages.with_raw_response.create(
        model=model,
        max_tokens=10,
        system=[{"type": "text", "text": "be brief", "cache_control": {"type": "ephemeral"}}],
        messages=[{"role": "user", "content": "hi"}],
    )
    m = raw.parse()
    assert raw.headers["x-request-id"].startswith("req_")
    assert m.type == "message" and m.role == "assistant"
    assert m.stop_reason == "max_tokens"
    assert m.content[0].type == "text" and len(m.content[0].text.split()) == 10
    assert m.usage.output_tokens == 10


@pytest.mark.parametrize("model", MODELS)
def test_stream_helper(anth, model):
    with anth.messages.stream(model=model, max_tokens=100, messages=[{"role": "user", "content": "hi"}]) as stream:
        text = "".join(stream.text_stream)
        final = stream.get_final_message()
    assert len(text.split()) == 32
    assert final.stop_reason == "end_turn"
    assert final.content[0].text == text
    assert final.usage.output_tokens == 32


@pytest.mark.parametrize("model", MODELS)
def test_tool_use_loop(anth, model):
    messages = [{"role": "user", "content": "weather in Rome?"}]
    first = anth.messages.create(model=model, max_tokens=100, tools=[WEATHER_ANTHROPIC], messages=messages)
    assert first.stop_reason == "tool_use"
    tool = next(b for b in first.content if b.type == "tool_use")
    assert tool.name == "get_weather" and tool.input == {"city": "mock"}

    messages += [
        {"role": "assistant", "content": first.content},
        {
            "role": "user",
            "content": [{"type": "tool_result", "tool_use_id": tool.id, "content": "rain"}],
        },
    ]
    second = anth.messages.create(model=model, max_tokens=100, tools=[WEATHER_ANTHROPIC], messages=messages)
    assert second.stop_reason == "end_turn"


@pytest.mark.parametrize("model", MODELS)
def test_streamed_tool_use(anth, model):
    with anth.messages.stream(
        model=model,
        max_tokens=100,
        tools=[WEATHER_ANTHROPIC],
        messages=[{"role": "user", "content": "weather?"}],
    ) as s:
        final = s.get_final_message()
    tool = final.content[0]
    assert final.stop_reason == "tool_use" and tool.type == "tool_use" and tool.input == {"city": "mock"}


def test_errors(anth, make_key):
    with pytest.raises(anthropic.AuthenticationError):
        anthropic.Anthropic(base_url=str(anth.base_url), api_key="nope", max_retries=0).messages.create(
            model="fast", max_tokens=5, messages=[{"role": "user", "content": "x"}]
        )
    with pytest.raises(anthropic.NotFoundError):
        anth.messages.create(
            model="no-such-model",
            max_tokens=5,
            messages=[{"role": "user", "content": "x"}],
        )
    with pytest.raises(anthropic.BadRequestError):
        anth.messages.create(model="fast", max_tokens=0, messages=[{"role": "user", "content": "x"}])

    limited = anthropic.Anthropic(base_url=str(anth.base_url), api_key=make_key(rpm=1), max_retries=0)
    limited.messages.create(model="fast", max_tokens=5, messages=[{"role": "user", "content": "x"}])
    with pytest.raises(anthropic.RateLimitError):
        limited.messages.create(model="fast", max_tokens=5, messages=[{"role": "user", "content": "x"}])


def test_exact_cache_hit_streams(anth):
    body = {"model": "fast", "max_tokens": 16, "messages": [{"role": "user", "content": "cache me please"}]}
    first = anth.messages.create(**body)
    for _ in range(50):  # the cache is filled off the request path
        raw = anth.messages.with_raw_response.create(**body)
        if raw.headers.get("x-switchyard-cache") == "exact":
            break
    else:
        pytest.fail("no cache hit")
    with anth.messages.stream(**body) as s:
        assert "".join(s.text_stream) == first.content[0].text
