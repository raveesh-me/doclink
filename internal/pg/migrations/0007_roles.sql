-- Role per service. Each service connects as its own role and is granted USAGE
-- on its own schema only.
--
-- This is what makes the isolation real rather than aspirational: the
-- subscriptions service physically cannot read pim.items, so its linkage table
-- cannot quietly grow a join to the anchor and the "no foreign keys across the
-- boundary" rule is enforced by Postgres rather than by code review.
--
-- Passwords are templated in by the migrator; see internal/pg/migrate.go.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_pim') THEN
        CREATE ROLE svc_pim LOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_subscriptions') THEN
        CREATE ROLE svc_subscriptions LOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_shipping') THEN
        CREATE ROLE svc_shipping LOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_doclink') THEN
        CREATE ROLE svc_doclink LOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_bench') THEN
        CREATE ROLE svc_bench LOGIN;
    END IF;
END $$;

-- Revoke the default: without this, PUBLIC can see every schema.
REVOKE ALL ON SCHEMA pim, subscriptions, shipping, doclink, monolith FROM PUBLIC;

GRANT USAGE ON SCHEMA pim TO svc_pim;
GRANT ALL ON ALL TABLES IN SCHEMA pim TO svc_pim;
GRANT ALL ON ALL SEQUENCES IN SCHEMA pim TO svc_pim;

GRANT USAGE ON SCHEMA subscriptions TO svc_subscriptions;
GRANT ALL ON ALL TABLES IN SCHEMA subscriptions TO svc_subscriptions;
GRANT ALL ON ALL SEQUENCES IN SCHEMA subscriptions TO svc_subscriptions;

GRANT USAGE ON SCHEMA shipping TO svc_shipping;
GRANT ALL ON ALL TABLES IN SCHEMA shipping TO svc_shipping;
GRANT ALL ON ALL SEQUENCES IN SCHEMA shipping TO svc_shipping;

GRANT USAGE ON SCHEMA doclink TO svc_doclink;
GRANT ALL ON ALL TABLES IN SCHEMA doclink TO svc_doclink;
GRANT ALL ON ALL SEQUENCES IN SCHEMA doclink TO svc_doclink;

-- The benchmark harness is the one component allowed to see across the
-- boundary, because measuring the monolith baseline requires exactly the
-- cross-cutting read that the architecture forbids in production code.
GRANT USAGE ON SCHEMA monolith, pim, subscriptions, shipping, doclink TO svc_bench;
GRANT ALL ON ALL TABLES IN SCHEMA monolith, pim, subscriptions, shipping, doclink TO svc_bench;
GRANT ALL ON ALL SEQUENCES IN SCHEMA monolith, pim, subscriptions, shipping, doclink TO svc_bench;
