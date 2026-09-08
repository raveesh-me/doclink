CREATE TABLE IF NOT EXISTS subscriptions.plans (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    interval      TEXT NOT NULL,
    discount_bps  INT NOT NULL DEFAULT 0,
    min_cycles    INT NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The linkage table, owned entirely by subscriptions.
--
-- anchor_ref is a DocRef string, not a foreign key. It cannot be a foreign key:
-- pim.items is in another schema this role cannot read, standing in for another
-- database owned by another team. Referential integrity is therefore this
-- service's own problem, handled by the lifecycle handler.
CREATE TABLE IF NOT EXISTS subscriptions.coverage (
    id                      TEXT PRIMARY KEY,
    plan_id                 TEXT NOT NULL REFERENCES subscriptions.plans(id) ON DELETE CASCADE,
    anchor_ref              TEXT NOT NULL,
    max_quantity_per_cycle  INT NOT NULL DEFAULT 1,
    prepaid_only            BOOLEAN NOT NULL DEFAULT false,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (plan_id, anchor_ref)
);

-- The traversal index. Every satellite needs one of these on anchor_ref; it is
-- what makes the federated fan-out affordable.
CREATE INDEX IF NOT EXISTS coverage_anchor ON subscriptions.coverage (anchor_ref);
