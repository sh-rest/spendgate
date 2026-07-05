-- 001_init: core tables for spendgate.

CREATE TABLE tenants (
    id               BIGSERIAL PRIMARY KEY,
    name             TEXT        NOT NULL,
    api_key_hash     TEXT        NOT NULL UNIQUE,
    fail_open        BOOLEAN     NOT NULL DEFAULT TRUE,
    monthly_budget_usd NUMERIC(12,4),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE model_prices (
    provider             TEXT           NOT NULL,
    model                TEXT           NOT NULL,
    input_usd_per_mtok   NUMERIC(12,4)  NOT NULL,
    output_usd_per_mtok  NUMERIC(12,4)  NOT NULL,
    PRIMARY KEY (provider, model)
);

CREATE TABLE requests (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     BIGINT      NOT NULL REFERENCES tenants(id),
    feature       TEXT        NOT NULL DEFAULT '',
    provider      TEXT        NOT NULL,
    model         TEXT        NOT NULL,
    input_tokens  INTEGER     NOT NULL DEFAULT 0,
    output_tokens INTEGER     NOT NULL DEFAULT 0,
    cost_usd      NUMERIC(12,6) NOT NULL DEFAULT 0,
    estimated     BOOLEAN     NOT NULL DEFAULT FALSE,
    status        INTEGER     NOT NULL DEFAULT 0,
    latency_ms    INTEGER     NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX requests_tenant_created_idx ON requests (tenant_id, created_at);
