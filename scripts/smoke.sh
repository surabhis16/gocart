#!/usr/bin/env bash
# End-to-end check of a RUNNING GoCart stack, using only curl.
#
#   ./scripts/smoke.sh                          # against docker compose (8080 / 8081)
#   PRODUCT=http://localhost:8080 ORDER=http://localhost:8081 ./scripts/smoke.sh
#
# It uses the demo catalogue the Product service seeds on first start, and it
# changes stock, so run it against a fresh stack:  docker compose down -v
#
# The failure-and-recovery demo (stop the Product service mid-order) is
# scripts/outage-demo.sh; this script only checks the paths that need both
# services up.
set -euo pipefail

PRODUCT="${PRODUCT:-http://localhost:8080}"
ORDER="${ORDER:-http://localhost:8081}"

pass=0
fail=0
body=""
status=""

req() { # method url [json-body] [extra curl args...]
  local method=$1 url=$2 data=${3:-}
  shift 3 || shift $#
  local out
  if [ -n "$data" ]; then
    out=$(curl -sS -X "$method" -H 'Content-Type: application/json' -d "$data" -w $'\n%{http_code}' "$@" "$url")
  else
    out=$(curl -sS -X "$method" -w $'\n%{http_code}' "$@" "$url")
  fi
  status=${out##*$'\n'}
  body=${out%$'\n'*}
}

jq_() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1], {"d": d}))' "$1" <<<"$body"; }

check() { # description expected actual
  if [ "$2" = "$3" ]; then
    pass=$((pass + 1)); printf '  ok    %s\n' "$1"
  else
    fail=$((fail + 1)); printf '  FAIL  %s\n        expected: %s\n        actual:   %s\n' "$1" "$2" "$3"
    printf '        body: %s\n' "$body"
  fi
}

echo "Waiting for both services..."
for url in "$PRODUCT" "$ORDER"; do
  for _ in $(seq 1 60); do
    curl -fsS "$url/healthz" >/dev/null 2>&1 && break
    sleep 1
  done
  curl -fsS "$url/healthz" >/dev/null || { echo "$url is not healthy"; exit 1; }
done

stock() { req GET "$PRODUCT/products/$1" ""; jq_ "d['stock']"; }

echo
echo "Place an order for 2 keyboards (stock 25) and 1 hub (stock 40)"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","items":[{"product_id":"prd_keyboard","quantity":2},{"product_id":"prd_hub","quantity":1}]}' -H 'Idempotency-Key: smoke-1'
check "201 Created" 201 "$status"
check "status is confirmed" confirmed "$(jq_ "d['status']")"
check "total is 2*499900 + 249900 paise" 1249700 "$(jq_ "d['total_minor']")"
order_id=$(jq_ "d['id']")
check "keyboard stock fell to 23" 23 "$(stock prd_keyboard)"
check "hub stock fell to 39" 39 "$(stock prd_hub)"

echo
echo "Retry the same request with the same Idempotency-Key"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","items":[{"product_id":"prd_keyboard","quantity":2},{"product_id":"prd_hub","quantity":1}]}' -H 'Idempotency-Key: smoke-1'
check "200 OK, not a second order" 200 "$status"
check "same order id" "$order_id" "$(jq_ "d['id']")"
check "keyboard stock still 23" 23 "$(stock prd_keyboard)"

echo
echo "Order more lamps than exist (stock 3)"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","items":[{"product_id":"prd_lamp","quantity":4}]}'
check "409 Conflict" 409 "$status"
check "problem type is order-rejected" urn:gocart:problem:order-rejected "$(jq_ "d['type']")"
check "reason is insufficient_stock" insufficient_stock "$(jq_ "d['reason']")"
check "lamp stock untouched" 3 "$(stock prd_lamp)"

echo
echo "Order a product that does not exist"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","items":[{"product_id":"prd_ghost","quantity":1}]}'
check "409 Conflict" 409 "$status"
check "reason is unknown_product" unknown_product "$(jq_ "d['reason']")"

echo
echo "Read the order back, and its event history"
req GET "$ORDER/orders/$order_id" ""
check "200 OK" 200 "$status"
check "version is 2" 2 "$(jq_ "d['version']")"
req GET "$ORDER/orders/$order_id/events" ""
check "history is OrderPlaced then OrderConfirmed" "OrderPlaced,OrderConfirmed" "$(jq_ "','.join(e['type'] for e in d['events'])")"

echo
echo "Cancel the order: stock must come back"
req POST "$ORDER/orders/$order_id/cancel" '{"reason":"smoke test"}'
check "200 OK" 200 "$status"
check "status is cancelled" cancelled "$(jq_ "d['status']")"
check "keyboard stock restored to 25" 25 "$(stock prd_keyboard)"
check "hub stock restored to 40" 40 "$(stock prd_hub)"
req GET "$ORDER/orders/$order_id/events" ""
check "history ends with StockReleased" "OrderPlaced,OrderConfirmed,OrderCancelled,StockReleased" "$(jq_ "','.join(e['type'] for e in d['events'])")"

echo
echo "Cancelling again is refused"
req POST "$ORDER/orders/$order_id/cancel" ''
check "409 Conflict" 409 "$status"
check "problem type is invalid-state" urn:gocart:problem:invalid-state "$(jq_ "d['type']")"

echo
echo "One request id follows a request through both services"
req GET "$ORDER/orders/ord_does_not_exist" "" -H 'X-Request-ID: smoke-trace-1'
check "404 Not Found" 404 "$status"
check "request id is echoed in the problem" smoke-trace-1 "$(jq_ "d['request_id']")"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
