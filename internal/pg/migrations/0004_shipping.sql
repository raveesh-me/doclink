CREATE TABLE IF NOT EXISTS shipping.profiles (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    carrier             TEXT NOT NULL,
    handling_days       INT NOT NULL DEFAULT 1,
    requires_signature  BOOLEAN NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Many-to-one on the anchor: an item has at most one shipping profile. The
-- cardinality is enforced here, by the service that actually cares about it,
-- rather than in the anchor's schema.
CREATE TABLE IF NOT EXISTS shipping.assignments (
    id             TEXT PRIMARY KEY,
    profile_id     TEXT NOT NULL REFERENCES shipping.profiles(id) ON DELETE CASCADE,
    anchor_ref     TEXT NOT NULL UNIQUE,
    weight_grams   INT NOT NULL DEFAULT 0,
    dimensions_cm  TEXT NOT NULL DEFAULT '',
    hazmat         BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS assignments_anchor ON shipping.assignments (anchor_ref);
