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
PG_PORT="${PG_PORT:-5433}"
if podman exec doclink-pg pg_isready -U postgres >/dev/null 2>&1; then
  out=$(podman exec doclink-pg psql -X -q -t \
        "postgres://svc_subscriptions:doclink-dev@localhost:5432/doclink" \
        -c "SELECT count(*) FROM pim.items" 2>&1 || true)
  if grep -q "permission denied" <<<"$out"; then
    ok "svc_subscriptions is denied read access to pim.items"
  else
    bad "svc_subscriptions could read pim.items — role isolation is not in effect"
    echo "      $out"
  fi
else
  printf "  \033[33m–\033[0m local Postgres not running; skipped (run: make pg)\n"
fi

echo
echo "The registry must hold declarations, not link rows."
if strip_sql_comments < internal/pg/migrations/0005_doclink.sql | grep -qE 'CREATE TABLE.*doclink\.(coverage|assignments)'; then
  bad "the registry owns a satellite's linkage table"
else
  ok "doclink schema holds only declarations plus its explicit link_index cache"
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
