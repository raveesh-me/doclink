-- The registry. It stores declarations about links; it does not store links.
-- The one exception is link_index, which is an explicit, opt-in cache used only
-- by the INDEXED strategy and is never a source of truth.

CREATE TABLE IF NOT EXISTS doclink.document_types (
    type_key           TEXT PRIMARY KEY,          -- "pim/item"
    namespace          TEXT NOT NULL,
    type               TEXT NOT NULL,
    display_name       TEXT NOT NULL,
    resolver_endpoint  TEXT NOT NULL,
    registered_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS doclink.extension_points (
    id                 TEXT PRIMARY KEY,          -- "pim/item:detail.card"
    anchor_type_key    TEXT NOT NULL,
    display_name       TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    registered_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A contribution is a satellite volunteering to fill a slot. There is no
-- approval step and no reference from the extension point back to here: the
-- host discovers contributions at runtime and cannot enumerate them at build
-- time. That asymmetry is the entire point.
CREATE TABLE IF NOT EXISTS doclink.contributions (
    id                  TEXT PRIMARY KEY,
    extension_point_id  TEXT NOT NULL,
    namespace           TEXT NOT NULL,
    title               TEXT NOT NULL,
    icon                TEXT NOT NULL DEFAULT '',
    embed_kind          TEXT NOT NULL DEFAULT 'IFRAME',
    embed_url           TEXT NOT NULL,
    service_endpoint    TEXT NOT NULL DEFAULT '',
    weight              INT NOT NULL DEFAULT 0,
    registered_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (extension_point_id, namespace)
);

CREATE INDEX IF NOT EXISTS contributions_slot
    ON doclink.contributions (extension_point_id, weight DESC);

CREATE TABLE IF NOT EXISTS doclink.link_types (
    id                       TEXT PRIMARY KEY,
    anchor_type_key          TEXT NOT NULL,       -- "pim/item"
    source_type_key          TEXT NOT NULL,       -- "subscriptions/plan"
    predicate                TEXT NOT NULL,
    cardinality              TEXT NOT NULL,
    resolver_endpoint        TEXT NOT NULL,
    subscribes_to_lifecycle  BOOLEAN NOT NULL DEFAULT false,
    registered_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (anchor_type_key, source_type_key, predicate)
);

-- The fan-out plan for a given anchor type is exactly this lookup.
CREATE INDEX IF NOT EXISTS link_types_anchor ON doclink.link_types (anchor_type_key);
CREATE INDEX IF NOT EXISTS link_types_lifecycle
    ON doclink.link_types (anchor_type_key)
    WHERE subscribes_to_lifecycle;

-- INDEXED strategy only. Satellites push here after their own write commits, so
-- entries are eventually consistent by construction. written_at exists so the
-- benchmark can measure the staleness window directly.
CREATE TABLE IF NOT EXISTS doclink.link_index (
    anchor_ref   TEXT NOT NULL,
    source_ref   TEXT NOT NULL,
    predicate    TEXT NOT NULL,
    cardinality  TEXT NOT NULL,
    namespace    TEXT NOT NULL,
    sequence     BIGSERIAL,
    written_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (anchor_ref, source_ref, predicate)
);

CREATE INDEX IF NOT EXISTS link_index_anchor ON doclink.link_index (anchor_ref);

-- Delivery bookkeeping for lifecycle fan-out, so a satellite that was down when
-- an item was deleted still gets told.
CREATE TABLE IF NOT EXISTS doclink.deliveries (
    event_id      TEXT NOT NULL,
    namespace     TEXT NOT NULL,
    endpoint      TEXT NOT NULL,
    subject_ref   TEXT NOT NULL,
    kind          TEXT NOT NULL,
    sequence      BIGINT NOT NULL,
    attempts      INT NOT NULL DEFAULT 0,
    delivered_at  TIMESTAMPTZ,
    last_error    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, namespace)
);

CREATE INDEX IF NOT EXISTS deliveries_pending
    ON doclink.deliveries (created_at)
    WHERE delivered_at IS NULL;
