-- The straw-man baseline, built exactly the way the experiment argues against:
-- every satellite's data folded into the anchor's own table.
--
-- The benchmark reads this to measure what the decoupling costs at read time.
-- Its real cost is not visible in a query plan: every column below arrived as a
-- migration on the busiest table in the company, authored by a team that does
-- not own the feature, and it can only be written by someone who already knows
-- every satellite that will ever exist.

CREATE TABLE IF NOT EXISTS monolith.plans (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    interval      TEXT NOT NULL,
    discount_bps  INT NOT NULL DEFAULT 0,
    min_cycles    INT NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS monolith.profiles (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    carrier             TEXT NOT NULL,
    handling_days       INT NOT NULL DEFAULT 1,
    requires_signature  BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS monolith.items (
    id           TEXT PRIMARY KEY,
    sku          TEXT NOT NULL UNIQUE,
    title        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    price_cents  BIGINT NOT NULL DEFAULT 0,
    currency     TEXT NOT NULL DEFAULT 'USD',
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Added by the shipping team, in the PIM team's table. Many-to-one, so it
    -- fits in a column and looks harmless.
    shipping_profile_id       TEXT REFERENCES monolith.profiles(id),
    shipping_weight_grams     INT NOT NULL DEFAULT 0,
    shipping_dimensions_cm    TEXT NOT NULL DEFAULT '',
    shipping_hazmat           BOOLEAN NOT NULL DEFAULT false
);

-- Subscriptions is many-to-many, so it does NOT fit in a column, and the
-- monolith is forced into a join table living in the anchor's schema. This is
-- the honest version of the baseline: the naive design does not even stay naive.
CREATE TABLE IF NOT EXISTS monolith.item_plans (
    item_id                 TEXT NOT NULL REFERENCES monolith.items(id) ON DELETE CASCADE,
    plan_id                 TEXT NOT NULL REFERENCES monolith.plans(id) ON DELETE CASCADE,
    max_quantity_per_cycle  INT NOT NULL DEFAULT 1,
    prepaid_only            BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (item_id, plan_id)
);

CREATE INDEX IF NOT EXISTS item_plans_item ON monolith.item_plans (item_id);
CREATE INDEX IF NOT EXISTS items_shipping_profile ON monolith.items (shipping_profile_id);
