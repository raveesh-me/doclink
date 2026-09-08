#!/usr/bin/env bash
#
# The architectural claims, as assertions.
#
# Every rule this repo argues for is checkable, and a rule that is not checked is
# a rule that will be broken by the third pull request. Run this in CI.
set -uo pipefail
cd "$(dirname "$0")/.."

PASS=0
FAIL=0
ok()   { printf "  \033[32m✓\033[0m %s\n" "$1"; PASS=$((PASS+1)); }
bad()  { printf "  \033[31m✗\033[0m %s\n" "$1"; FAIL=$((FAIL+1)); }

# Strip // line comments, /* */ blocks, <!-- --> blocks and Vue/TS doc comments
# so a rule can be *explained* in prose without tripping the check on itself.
strip_comments() {
  perl -0777 -pe 's{/\*.*?\*/}{}gs; s{<!--.*?-->}{}gs; s{^\s*(//|\*|#).*$}{}gm'
}

# SQL uses -- for comments, and these migrations explain the rules at length in
# prose that names the very things the rules forbid.
strip_sql_comments() { perl -pe 's{--.*$}{}' ; }

echo
echo "PIM is the anchor. It must not know its satellites."
for term in subscriptions shipping synth marketplace; do
  hits=$(cat services/pim/*.go web/pim-web/src/*.ts web/pim-web/src/**/*.vue 2>/dev/null \
          | strip_comments | grep -in "$term" || true)
  if [[ -z "$hits" ]]; then
    ok "no reference to '$term' in PIM service or shell (comments excluded)"
  else
    bad "PIM references '$term':"; echo "$hits" | sed 's/^/      /'
  fi
done

echo
echo "The anchor's table must carry no satellite columns."
pim_sql=$(strip_sql_comments < internal/pg/migrations/0002_pim.sql)
if grep -qiE '(subscription|shipping|plan_id|profile_id)' <<<"$pim_sql"; then
  bad "0002_pim.sql declares a satellite column"
  grep -inE '(subscription|shipping|plan_id|profile_id)' <<<"$pim_sql" | sed 's/^/      /'
else
  ok "pim.items has no satellite column"
fi

echo
echo "The anchor's proto must not import a satellite's."
if grep -q 'import' proto/pim/v1/item.proto | grep -vq 'google/protobuf'; then
  bad "pim/v1/item.proto imports something beyond well-known types"
else
  ok "pim/v1/item.proto imports only well-known types"
fi

echo
echo "Satellites must link by DocRef string, never by foreign key to the anchor."
for f in internal/pg/migrations/0003_subscriptions.sql internal/pg/migrations/0004_shipping.sql; do
  if strip_sql_comments < "$f" | grep -iE 'REFERENCES\s+pim\.' >/dev/null; then
    bad "$(basename "$f") has a foreign key into pim"
  else
    ok "$(basename "$f") holds anchor_ref as TEXT, not a foreign key"
  fi
done

echo
echo "Postgres must enforce the schema boundary, not convention."
# Reachable two ways: a psql on PATH talking to PGHOST/PGPORT (how CI runs it,
# against a service container), or the podman container from `make pg` (how a
# laptop runs it). Skipped rather than failed when neither is up, so the other
# ten checks still run in a bare checkout.
PG_HOST="${PGHOST:-localhost}"
PG_PORT="${PGPORT:-5433}"
DENIED_DSN="postgres://svc_subscriptions:${SERVICE_ROLE_PASSWORD:-doclink-dev}@${PG_HOST}:${PG_PORT}/doclink"

probe() { psql -X -q -t "$1" -c "SELECT count(*) FROM pim.items" 2>&1; }

out=""
if command -v psql >/dev/null 2>&1 && pg_isready -h "$PG_HOST" -p "$PG_PORT" >/dev/null 2>&1; then
  out=$(probe "$DENIED_DSN" || true)
elif podman exec doclink-pg pg_isready -U postgres >/dev/null 2>&1; then
  # Inside the container Postgres is on its own 5432, not the host mapping.
  out=$(podman exec doclink-pg psql -X -q -t \
        "postgres://svc_subscriptions:doclink-dev@localhost:5432/doclink" \
        -c "SELECT count(*) FROM pim.items" 2>&1 || true)
elif [[ -n "${REQUIRE_POSTGRES:-}" ]]; then
  # Skipping is right on a laptop with nothing running. In CI it is the worst
  # possible outcome: a green check that silently asserted nothing. Callers that
  # provision a database set REQUIRE_POSTGRES and get a failure instead.
  bad "REQUIRE_POSTGRES is set but no Postgres was reachable at ${PG_HOST}:${PG_PORT}"
else
  printf "  \033[33m–\033[0m no Postgres reachable; skipped (run: make pg)\n"
fi

if [[ -n "$out" ]]; then
  if grep -q "permission denied" <<<"$out"; then
    ok "svc_subscriptions is denied read access to pim.items"
  else
    bad "svc_subscriptions could read pim.items — role isolation is not in effect"
    echo "      $out"
  fi
fi

echo
echo "The registry must hold declarations, not link rows."
if strip_sql_comments < internal/pg/migrations/0005_doclink.sql | grep -qE 'CREATE TABLE.*doclink\.(coverage|assignments)'; then
  bad "the registry owns a satellite's linkage table"
else
  ok "doclink schema holds only declarations plus its explicit link_index cache"
fi

echo
echo "Embeddable origins must be constrained by config the registry cannot write."
if grep -q 'ALLOWED_EMBED_ORIGINS' deploy/manifests/04-web.yaml; then
  ok "the shell deployment sets ALLOWED_EMBED_ORIGINS"
else
  bad "no ALLOWED_EMBED_ORIGINS in the shell deployment: frame-src would be derived"
  bad "  purely from the registry, so a rogue row would allow its own origin"
fi
if grep -qE 'allowedPatterns|p\.allowed\(' services/webhost/csp.go; then
  ok "webhost filters registry origins through the configured allowlist"
else
  bad "webhost no longer filters registry origins"
fi

echo
echo "The host must never postMessage to a wildcard origin with domain data."
if grep -n 'postMessage(' web/host-sdk/src/host.ts | grep -q '"\*"'; then
  bad "host.ts posts to a wildcard origin"
else
  ok "host.ts always targets the guest's specific origin"
fi

echo
printf "%d passed, %d failed\n\n" "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
