#!/usr/bin/env bash
#
# Local bring-up: four Go services and three Vite dev servers on the host,
# against the podman Postgres from `make pg`.
#
# The cluster path (`make cluster`) is the honest one — it gives each component
# its own origin. This script trades that for a fast edit/reload loop, using
# distinct ports as the closest available stand-in: localhost:5181 and
# localhost:5182 are still different origins as far as the browser is concerned,
# so the postMessage handshake and the sandbox are genuinely exercised.
set -euo pipefail

cd "$(dirname "$0")/.."

PG_PORT="${PG_PORT:-5433}"
LOG_DIR="${LOG_DIR:-.logs}"
mkdir -p "$LOG_DIR"

dsn() { echo "postgres://$1:doclink-dev@localhost:${PG_PORT}/doclink?sslmode=disable"; }

if ! pg_isready -h localhost -p "$PG_PORT" >/dev/null 2>&1; then
  if ! podman exec doclink-pg pg_isready -U postgres >/dev/null 2>&1; then
    echo "Postgres is not up on ${PG_PORT}. Run: make pg" >&2
    exit 1
  fi
fi

echo "stopping anything already running…"
pkill -f 'bin/(doclink|pim|subscriptions|shipping)' 2>/dev/null || true
pkill -f 'vite' 2>/dev/null || true
sleep 1

start() {
  local name="$1"; shift
  echo "  $name"
  env "$@" > "$LOG_DIR/$name.log" 2>&1 &
}

echo "starting backends…"
start doclink ADDR=:8080 \
  DOCLINK_DSN="$(dsn svc_doclink)" BENCH_DSN="$(dsn svc_bench)" ./bin/doclink
start pim ADDR=:8081 SELF_ENDPOINT=http://localhost:8081 \
  DOCLINK_ENDPOINT=http://localhost:8080 PIM_DSN="$(dsn svc_pim)" ./bin/pim
start subscriptions ADDR=:8082 SELF_ENDPOINT=http://localhost:8082 \
  DOCLINK_ENDPOINT=http://localhost:8080 EMBED_BASE_URL=http://localhost:5182 \
  SUBSCRIPTIONS_DSN="$(dsn svc_subscriptions)" ./bin/subscriptions
start shipping ADDR=:8083 SELF_ENDPOINT=http://localhost:8083 \
  DOCLINK_ENDPOINT=http://localhost:8080 EMBED_BASE_URL=http://localhost:5183 \
  SHIPPING_DSN="$(dsn svc_shipping)" ./bin/shipping

echo "starting frontends…"
(cd web/pim-web && npx vite --port 5181 --strictPort) > "$LOG_DIR/pim-web.log" 2>&1 &
(cd web/subscriptions-web && npx vite --port 5182 --strictPort) > "$LOG_DIR/subs-web.log" 2>&1 &
(cd web/shipping-web && npx vite --port 5183 --strictPort) > "$LOG_DIR/ship-web.log" 2>&1 &

sleep 4
echo
for p in 8080 8081 8082 8083; do
  printf "  :%s  %s\n" "$p" "$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$p/healthz")"
done
echo
echo "  open http://localhost:5181/items"
echo "  logs in $LOG_DIR/"
