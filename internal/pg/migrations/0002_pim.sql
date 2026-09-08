-- The anchor. Note the absence of any column referring to a satellite. This
-- table is the thing the experiment is protecting: adding subscriptions,
-- shipping, or a marketplace connector must not produce a migration here.

CREATE TABLE IF NOT EXISTS pim.items (
    id           TEXT PRIMARY KEY,
    sku          TEXT NOT NULL UNIQUE,
    title        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    price_cents  BIGINT NOT NULL DEFAULT 0,
    currency     TEXT NOT NULL DEFAULT 'USD',
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS items_title_trgm ON pim.items (lower(title) text_pattern_ops);
CREATE INDEX IF NOT EXISTS items_status ON pim.items (status);

-- Transactional outbox. Replaces ON DELETE CASCADE, which cannot exist across
-- a database boundary. Written in the same transaction as the mutation, so an
-- item can never be deleted without an event being durably queued.
CREATE TABLE IF NOT EXISTS pim.outbox (
    event_id     TEXT PRIMARY KEY,
    subject_ref  TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('CREATED','UPDATED','DELETED')),
    sequence     BIGINT NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT
);

-- The relay polls this. Partial index keeps the hot path proportional to the
-- unpublished backlog rather than to total history.
CREATE INDEX IF NOT EXISTS outbox_unpublished
    ON pim.outbox (occurred_at)
    WHERE published_at IS NULL;

-- Per-subject monotonic sequence, so a satellite can discard a redelivered or
-- out-of-order event without coordinating with anyone.
CREATE TABLE IF NOT EXISTS pim.sequences (
    subject_ref TEXT PRIMARY KEY,
    value       BIGINT NOT NULL DEFAULT 0
);
