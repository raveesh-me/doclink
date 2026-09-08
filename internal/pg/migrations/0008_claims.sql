-- Claim columns for the two queue drains.
--
-- Both PIM's outbox relay and the registry's delivery worker claim a batch with
-- a single UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED), then do
-- the network call outside any transaction. Without a claim column that pattern
-- has nothing to write, and FOR UPDATE SKIP LOCKED in a bare SELECT releases its
-- locks the moment the statement ends — so two replicas would happily publish
-- the same event.
--
-- The stale-claim window (30 seconds, in the queries) is what makes a relay
-- crashing mid-batch recoverable: its claims age out and another replica picks
-- the rows up. Redelivery is therefore expected, which is why every lifecycle
-- handler is required to be idempotent.

ALTER TABLE pim.outbox ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;
ALTER TABLE doclink.deliveries ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;

-- Replaces the older partial indexes: the drain now filters on claimed_at too,
-- so the index has to cover it or every poll degrades to a sequential scan as
-- history accumulates.
DROP INDEX IF EXISTS pim.outbox_unpublished;
CREATE INDEX IF NOT EXISTS outbox_claimable
    ON pim.outbox (occurred_at)
    WHERE published_at IS NULL;

DROP INDEX IF EXISTS doclink.deliveries_pending;
CREATE INDEX IF NOT EXISTS deliveries_claimable
    ON doclink.deliveries (created_at)
    WHERE delivered_at IS NULL;
