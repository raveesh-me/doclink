-- One CNPG cluster, one database, one schema per service, one role per schema.
--
-- This is a laptop concession over a Postgres cluster per service. The boundary
-- it is standing in for is still enforced by the database rather than by
-- convention: each service connects as its own role and is granted USAGE on its
-- own schema only. A cross-schema join is not "discouraged" here, it fails.

CREATE SCHEMA IF NOT EXISTS pim;
CREATE SCHEMA IF NOT EXISTS subscriptions;
CREATE SCHEMA IF NOT EXISTS shipping;
CREATE SCHEMA IF NOT EXISTS doclink;

-- The straw-man baseline lives in its own schema so it can be benchmarked
-- side by side without contaminating the real design.
CREATE SCHEMA IF NOT EXISTS monolith;
