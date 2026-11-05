#!/usr/bin/env bash
# What happens when a service is down while a customer orders?
#
#   ./scripts/outage-demo.sh                    # against docker compose
#
# Two acts. In each, one dependency goes away mid-checkout and the customer
# still gets an honest answer; the order then finishes by itself when the
# dependency returns, with nothing done twice.
#   Act 1: the Product service is down (no stock can be reserved yet).
#   Act 2: the Payment service is down (stock is held, money not yet taken).
#
# Needs a fresh stack:  docker compose down -v && docker compose up -d --build
#
# Override how services are stopped and started to use this outside compose:
#   STOP_PRODUCT='kubectl -n gocart-staging scale deploy/product-service --replicas=0' \
#   START_PRODUCT='kubectl -n gocart-staging scale deploy/product-service --replicas=1' \
#   (and STOP_PAYMENT / START_PAYMENT likewise) ./scripts/outage-demo.sh
set -euo pipefail

PRODUCT="${PRODUCT:-http://localhost:8080}"
ORDER="${ORDER:-http://localhost:8081}"
PAYMENT="${PAYMENT:-http://localhost:8082}"
ADMIN_KEY="${ADMIN_KEY:-dev-admin-key}"
STOP_PRODUCT="${STOP_PRODUCT:-docker compose stop product-service}"
START_PRODUCT="${START_PRODUCT:-docker compose start product-service}"
STOP_PAYMENT="${STOP_PAYMENT:-docker compose stop payment-service}"
START_PAYMENT="${START_PAYMENT:-docker compose start payment-service}"
# The Order service reconciles every RECONCILE_INTERVAL (5s default) once an
# order has waited 10s; give it comfortably longer than that.
SETTLE_SECONDS="${SETTLE_SECONDS:-45}"
RUN="$(date +%s)"
BODY="$(mktemp)"
trap 'rm -f "$BODY"' EXIT

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
field() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1], {"d": d}))' "$1"; }
wait_healthy() {
  for _ in $(seq 1 60); do curl -fsS "$1/healthz" >/dev/null 2>&1 && return 0; sleep 1; done
  echo "$1 did not become healthy" >&2; return 1
}
stock() { curl -fsS "$PRODUCT/products/$1" | field "d['stock']"; }

# order_during_outage KEY PRODUCT_ID -> places an order, expects 202, sets $id
order_during_outage() {
  local key=$1 prd=$2
  local code
  code=$(curl -sS -o "$BODY" -w '%{http_code}' -X POST "$ORDER/orders" \
    -H 'Content-Type: application/json' -H "Idempotency-Key: $key" \
    -d "{\"customer_id\":\"cust_demo\",\"payment_token\":\"tok_visa\",\"items\":[{\"product_id\":\"$prd\",\"quantity\":3}]}")
  id=$(field "d['id']" <"$BODY")
  echo "   HTTP $code, status: $(field "d['status']" <"$BODY"), id: $id"
  [ "$code" = "202" ] || { echo "expected 202 Accepted" >&2; exit 1; }
}

# wait_for_confirmed -> polls $id until it leaves placed / awaiting_payment
wait_for_confirmed() {
  local status=placed i
  for i in $(seq 1 "$SETTLE_SECONDS"); do
    status=$(curl -fsS "$ORDER/orders/$id" | field "d['status']")
    case "$status" in placed|awaiting_payment) sleep 1 ;; *) break ;; esac
  done
  echo "   status after ${i}s: $status"
  [ "$status" = "confirmed" ] || { echo "order did not settle" >&2; exit 1; }
}

history() {
  curl -fsS -H "Authorization: Bearer $ADMIN_KEY" "$ORDER/orders/$id/events" | python3 -c '
import json, sys
for e in json.load(sys.stdin)["events"]:
    print("   v%d  %-16s %s" % (e["version"], e["type"], e["at"]))'
}

wait_healthy "$PRODUCT"; wait_healthy "$ORDER"; wait_healthy "$PAYMENT"

# ---------------------------------------------------------------------------
say "ACT 1: the Product service is down"
before=$(stock prd_notebook)
echo "   Notebook stock before: $before"

say "1. Stop the Product service, then place an order"
eval "$STOP_PRODUCT" >/dev/null 2>&1
order_during_outage "outage-product-$RUN" prd_notebook
echo "   The order was accepted and recorded. It is not lost, and it is not pretending to be confirmed."

say "2. Start the Product service again and wait for the order to finish by itself"
eval "$START_PRODUCT" >/dev/null 2>&1
wait_healthy "$PRODUCT"
wait_for_confirmed

say "3. History: placed, stock reserved, then paid"
history
after=$(stock prd_notebook)
echo "   Notebook stock: $before -> $after (expected $((before - 3)))"
[ "$after" -eq $((before - 3)) ] || { echo "stock is wrong" >&2; exit 1; }

say "4. Retrying the original request is safe"
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$ORDER/orders" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: outage-product-$RUN" \
  -d '{"customer_id":"cust_demo","payment_token":"tok_visa","items":[{"product_id":"prd_notebook","quantity":3}]}')
after2=$(stock prd_notebook)
echo "   HTTP $code (the same order, not a new one); stock still $after2"
[ "$code" = "200" ] && [ "$after2" -eq "$after" ] || { echo "retry was not idempotent" >&2; exit 1; }

# ---------------------------------------------------------------------------
say "ACT 2: the Payment service is down"
before=$(stock prd_bottle)
echo "   Bottle stock before: $before"

say "5. Stop the Payment service, then place an order"
eval "$STOP_PAYMENT" >/dev/null 2>&1
order_during_outage "outage-payment-$RUN" prd_bottle
echo "   Stock is held, but the customer has not been charged and the order is not confirmed."

say "6. Start the Payment service again and wait for the order to finish by itself"
eval "$START_PAYMENT" >/dev/null 2>&1
wait_healthy "$PAYMENT"
wait_for_confirmed

say "7. History, and exactly one charge"
history
curl -fsS -H "Authorization: Bearer $ADMIN_KEY" "$PAYMENT/charges/$id" | python3 -c '
import json, sys
c = json.load(sys.stdin)
print("   charge: %s  %s  %d paise" % (c["reference"], c["status"], c["amount_minor"]))
assert c["status"] == "approved"'
after=$(stock prd_bottle)
echo "   Bottle stock: $before -> $after (expected $((before - 3)))"
[ "$after" -eq $((before - 3)) ] || { echo "stock is wrong" >&2; exit 1; }

say "Done: both outages, both recoveries and the retry behaved."
