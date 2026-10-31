"""Fixtures: a gateway URL, the demo key, and keys with tight limits created
through the admin API. Point SWITCHYARD_URL at a gateway whose `fast` and
`smart` aliases are served by the mock provider (configs/demo.yaml)."""

import os
import uuid

import anthropic
import httpx
import openai
import pytest

URL = os.environ.get("SWITCHYARD_URL", "http://localhost:8080")
KEY = os.environ.get("SWITCHYARD_KEY", "sy_demo0000_demodemodemodemodemodemodemodemo")
ADMIN_URL = os.environ.get("SWITCHYARD_ADMIN_URL", "http://localhost:9091")
ADMIN_TOKEN = os.environ.get("SWITCHYARD_ADMIN_TOKEN", "dev-admin-token")


@pytest.fixture
def oai() -> openai.OpenAI:
    return openai.OpenAI(base_url=URL + "/v1", api_key=KEY, max_retries=0)


@pytest.fixture
def anth() -> anthropic.Anthropic:
    return anthropic.Anthropic(base_url=URL, api_key=KEY, max_retries=0)


@pytest.fixture
def make_key():
    """Create a throwaway key with the given limits; returns its plaintext."""

    def make(**limits) -> str:
        admin = httpx.Client(base_url=ADMIN_URL, headers={"Authorization": f"Bearer {ADMIN_TOKEN}"})
        tenant = "conformance"
        admin.put(f"/admin/api/tenants/{tenant}", json={"name": tenant}).raise_for_status()
        r = admin.post(
            "/admin/api/keys",
            json={
                "tenant_id": tenant,
                "name": f"test-{uuid.uuid4().hex[:6]}",
                **limits,
            },
        )
        r.raise_for_status()
        return r.json()["key"]

    return make


WEATHER_OPENAI = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Current weather for a city",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    },
}
WEATHER_ANTHROPIC = {
    "name": "get_weather",
    "description": "Current weather for a city",
    "input_schema": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"],
    },
}
