CREATE TABLE tenants (
    id              text PRIMARY KEY,
    name            text NOT NULL,
    -- Prompt and completion capture is off unless a tenant opts in.
    capture_prompts boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id                 text PRIMARY KEY,          -- public lookup id, part of the key string
    tenant_id          text NOT NULL REFERENCES tenants(id),
    name               text NOT NULL,
    secret_hash        bytea NOT NULL,            -- SHA-256 of the secret part; the secret is never stored
    rpm                integer CHECK (rpm >= 0),  -- NULL = gateway default
    tpm                integer CHECK (tpm >= 0),
    monthly_budget_usd numeric(14, 4) CHECK (monthly_budget_usd >= 0), -- NULL = no budget
    soft_budget_ratio  real NOT NULL DEFAULT 0.8 CHECK (soft_budget_ratio > 0 AND soft_budget_ratio <= 1),
    allowed_models     text[],                    -- NULL = any model
    created_at         timestamptz NOT NULL DEFAULT now(),
    revoked_at         timestamptz
);
CREATE INDEX api_keys_tenant ON api_keys (tenant_id);

CREATE TABLE usage_records (
    id                 bigserial PRIMARY KEY,
    ts                 timestamptz NOT NULL DEFAULT now(),
    request_id         text NOT NULL,
    key_id             text NOT NULL,
    tenant_id          text NOT NULL,
    surface            text NOT NULL,             -- openai | anthropic
    alias              text NOT NULL,
    provider           text NOT NULL DEFAULT '',
    model              text NOT NULL DEFAULT '',
    stream             boolean NOT NULL,
    status             integer NOT NULL,
    cache              text NOT NULL DEFAULT '',   -- '' | exact | semantic
    attempts           integer NOT NULL DEFAULT 0,
    fallback_depth     integer NOT NULL DEFAULT 0,
    input_tokens       integer NOT NULL DEFAULT 0,
    output_tokens      integer NOT NULL DEFAULT 0,
    cache_read_tokens  integer NOT NULL DEFAULT 0,
    cache_write_tokens integer NOT NULL DEFAULT 0,
    cost_usd           numeric(18, 10) NOT NULL DEFAULT 0,
    price_version      text NOT NULL DEFAULT '',
    latency_ms         integer NOT NULL DEFAULT 0,
    ttft_ms            integer
);
CREATE INDEX usage_records_key_ts ON usage_records (key_id, ts);
CREATE INDEX usage_records_ts ON usage_records (ts);
