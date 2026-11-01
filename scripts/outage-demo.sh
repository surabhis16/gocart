#!/usr/bin/env bash
# What happens when the Product service is down while a customer orders?
#
#   ./scripts/outage-demo.sh                    # against docker compose
#
# Needs a fresh stack:  docker compose down -v && docker compose up -d --build
#
# Override how the Product service is stopped and started to use this outside
# compose (for example, with plain processes or kubectl):
#   STOP_PRODUCT='kubectl -n gocart-staging scale deploy/product-service --replicas=0' \
#   START_PRODUCT='kubectl -n gocart-staging scale deploy/product-service --replicas=1' \
#   ./scripts/outage-demo.sh
set -euo pipefail

PRODUCT="${PRODUCT:-http://localhost:8080}"
ORDER="${ORDER:-http://localhost:8081}"
STOP_PRODUCT="${STOP_PRODUCT:-docker compose stop product-service}"
START_PRODUCT="${START_PRODUCT:-docker compose start product-service}"
# Order service reconciles every RECONCILE_INTERVAL (5s default) once an order
# has waited 10s; give it comfortably longer than that.
SETTLE_SECONDS="${SETTLE_SECONDS:-45}"

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
field() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1], {"d": d}))' "$1"; }
wait_healthy() {
  for _ in $(seq 1 60); do curl -fsS "$1/healthz" >/dev/null 2>&1 && return 0; sleep 1; done
  echo "$1 did not become healthy" >&2; return 1
}

wait_healthy "$PRODUCT"; wait_healthy "$ORDER"
before=$(curl -fsS "$PRODUCT/products/prd_notebook" | field "d['stock']")
say "Notebook stock before: $before"

say "1. Stop the Product service"
eval "$STOP_PRODUCT" >/dev/null 2>&1

say "2. Place an order while it is down"
resp=$(curl -sS -D - -o /tmp/outage-body.json -X POST "$ORDER/orders" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: outage-demo' \
  -d '{"customer_id":"cust_demo","items":[{"product_id":"prd_notebook","quantity":3}]}')
code=$(printf '%s' "$resp" | head -1 | awk '{print $2}')
id=$(field "d['id']" < /tmp/outage-body.json)
echo "   HTTP $code, status: $(field "d['status']" < /tmp/outage-body.json), id: $id"
echo "   The order was accepted and recorded. It is not lost, and it is not pretending to be confirmed."
[ "$code" = "202" ] || { echo "expected 202 Accepted" >&2; exit 1; }

say "3. Start the Product service again"
eval "$START_PRODUCT" >/dev/null 2>&1
wait_healthy "$PRODUCT"

say "4. Wait for the Order service to finish the order by itself (up to ${SETTLE_SECONDS}s)"
status=placed
for i in $(seq 1 "$SETTLE_SECONDS"); do
  status=$(curl -fsS "$ORDER/orders/$id" | field "d['status']")
  [ "$status" != "placed" ] && break
  sleep 1
done
echo "   status after ${i}s: $status"
[ "$status" = "confirmed" ] || { echo "order did not settle" >&2; exit 1; }

say "5. The order's history shows exactly what happened"
curl -fsS "$ORDER/orders/$id/events" | python3 -c '
import json, sys
for e in json.load(sys.stdin)["events"]:
    print("   v%d  %-15s %s" % (e["version"], e["type"], e["at"]))'

say "6. Stock was reserved exactly once"
after=$(curl -fsS "$PRODUCT/products/prd_notebook" | field "d['stock']")
echo "   Notebook stock: $before -> $after (expected $((before - 3)))"
[ "$after" -eq $((before - 3)) ] || { echo "stock is wrong" >&2; exit 1; }

say "7. Retrying the original request is safe"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$ORDER/orders" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: outage-demo' \
  -d '{"customer_id":"cust_demo","items":[{"product_id":"prd_notebook","quantity":3}]}')
after2=$(curl -fsS "$PRODUCT/products/prd_notebook" | field "d['stock']")
echo "   HTTP $code (the same order, not a new one); stock still $after2"
[ "$code" = "200" ] && [ "$after2" -eq "$after" ] || { echo "retry was not idempotent" >&2; exit 1; }

say "Done: outage, recovery and retry all behaved."
