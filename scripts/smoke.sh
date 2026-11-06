#!/usr/bin/env bash
# End-to-end check of a RUNNING GoCart stack, using only curl.
#
#   ./scripts/smoke.sh                          # against docker compose (8080-8083)
#   PRODUCT=... ORDER=... PAYMENT=... CART=... ADMIN_KEY=... ./scripts/smoke.sh
#
# It uses the demo catalogue the Product service seeds on first start, and it
# changes stock, so run it against a fresh stack:  docker compose down -v
#
# The failure-and-recovery demos are scripts/outage-demo.sh (a service goes
# away mid-order) and scripts/oversell-demo.sh (many buyers, little stock);
# this script only checks the paths that need every service up.
set -euo pipefail

PRODUCT="${PRODUCT:-http://localhost:8080}"
ORDER="${ORDER:-http://localhost:8081}"
PAYMENT="${PAYMENT:-http://localhost:8082}"
CART="${CART:-http://localhost:8083}"
# Must match the ADMIN_KEY the services were started with (compose defaults to this).
ADMIN_KEY="${ADMIN_KEY:-dev-admin-key}"
ADMIN=(-H "Authorization: Bearer $ADMIN_KEY")

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

echo "Waiting for all four services..."
for url in "$PRODUCT" "$ORDER" "$PAYMENT" "$CART"; do
  for _ in $(seq 1 60); do
    curl -fsS "$url/healthz" >/dev/null 2>&1 && break
    sleep 1
  done
  curl -fsS "$url/healthz" >/dev/null || { echo "$url is not healthy"; exit 1; }
done

stock() { req GET "$PRODUCT/products/$1" ""; jq_ "d['stock']"; }
charge_status() { req GET "$PAYMENT/charges/$1" "" "${ADMIN[@]}"; jq_ "d['status']"; }
types() { req GET "$ORDER/orders/$1/events" "" "${ADMIN[@]}"; jq_ "','.join(e['type'] for e in d['events'])"; }

echo
echo "Place an order for 2 keyboards (stock 25) and 1 hub (stock 40)"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","payment_token":"tok_visa","items":[{"product_id":"prd_keyboard","quantity":2},{"product_id":"prd_hub","quantity":1}]}' -H 'Idempotency-Key: smoke-1'
check "201 Created" 201 "$status"
check "status is confirmed" confirmed "$(jq_ "d['status']")"
check "total is 2*499900 + 249900 paise" 1249700 "$(jq_ "d['total_minor']")"
order_id=$(jq_ "d['id']")
check "keyboard stock fell to 23" 23 "$(stock prd_keyboard)"
check "hub stock fell to 39" 39 "$(stock prd_hub)"

echo
echo "Retry the same request with the same Idempotency-Key"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","payment_token":"tok_visa","items":[{"product_id":"prd_keyboard","quantity":2},{"product_id":"prd_hub","quantity":1}]}' -H 'Idempotency-Key: smoke-1'
check "200 OK, not a second order" 200 "$status"
check "same order id" "$order_id" "$(jq_ "d['id']")"
check "keyboard stock still 23" 23 "$(stock prd_keyboard)"

echo
echo "Order more lamps than exist (stock 3)"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","payment_token":"tok_visa","items":[{"product_id":"prd_lamp","quantity":4}]}'
check "409 Conflict" 409 "$status"
check "problem type is order-rejected" urn:gocart:problem:order-rejected "$(jq_ "d['type']")"
check "reason is insufficient_stock" insufficient_stock "$(jq_ "d['reason']")"
check "lamp stock untouched" 3 "$(stock prd_lamp)"

echo
echo "Order a product that does not exist"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","payment_token":"tok_visa","items":[{"product_id":"prd_ghost","quantity":1}]}'
check "409 Conflict" 409 "$status"
check "reason is unknown_product" unknown_product "$(jq_ "d['reason']")"

echo
echo "Read the order back, and its event history"
req GET "$ORDER/orders/$order_id" ""
check "200 OK" 200 "$status"
check "version is 3" 3 "$(jq_ "d['version']")"
check "history is placed, stock reserved, payment captured" "OrderPlaced,StockReserved,PaymentCaptured" "$(types "$order_id")"
check "the payment service holds an approved charge" approved "$(charge_status "$order_id")"
check "the payment token is never returned" False "$(python3 -c 'import sys; print("tok_" in sys.argv[1])' "$body")"

echo
echo "A declined card: a definite no, and the stock goes straight back"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","payment_token":"tok_declined","items":[{"product_id":"prd_lamp","quantity":1}]}'
check "402 Payment Required" 402 "$status"
check "problem type is payment-declined" urn:gocart:problem:payment-declined "$(jq_ "d['type']")"
declined_id=$(jq_ "d['order_id']")
check "lamp stock was given back" 3 "$(stock prd_lamp)"
req GET "$ORDER/orders/$declined_id" ""
check "the order is recorded as payment_failed" payment_failed "$(jq_ "d['status']")"

echo
echo "A flaky card network: the first attempt fails, the retry succeeds, one charge"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","payment_token":"tok_flaky","items":[{"product_id":"prd_lamp","quantity":1}]}'
check "201 Created despite the failed first attempt" 201 "$status"
flaky_id=$(jq_ "d['id']")
check "exactly one charge exists" approved "$(charge_status "$flaky_id")"
req POST "$ORDER/orders/$flaky_id/cancel" ''
check "lamp stock restored after cancelling it" 3 "$(stock prd_lamp)"

echo
echo "A card number instead of a token is refused before anything is stored"
req POST "$ORDER/orders" '{"customer_id":"cust_smoke","payment_token":"4242424242424242","items":[{"product_id":"prd_lamp","quantity":1}]}'
check "422 Unprocessable" 422 "$status"
check "the number is not echoed back" False "$(python3 -c 'import sys; print("4242" in sys.argv[1])' "$body")"

echo
echo "Admin operations need the key"
req POST "$ORDER/orders/$order_id/ship" ''
check "shipping without a key is 401" 401 "$status"
req POST "$ORDER/orders/$order_id/deliver" ''
check "delivering without a key is 401" 401 "$status"
req GET "$ORDER/orders/$order_id/events" ""
check "reading the event log without a key is 401" 401 "$status"
req POST "$PRODUCT/products" '{"name":"Cable","price_minor":19900,"stock":7}'
check "creating a product without a key is 401" 401 "$status"
req GET "$PAYMENT/charges/$order_id" ""
check "reading a charge without a key is 401" 401 "$status"

echo
echo "Every service exposes metrics"
for svc in "$PRODUCT" "$ORDER" "$PAYMENT" "$CART"; do
  req GET "$svc/metrics" ""
  check "$svc/metrics is 200" 200 "$status"
  check "$svc/metrics has request counters" True "$(python3 -c 'import sys; print("gocart_http_requests_total" in sys.argv[1])' "$body")"
done

echo
echo "Cancel the order: stock must come back"
req POST "$ORDER/orders/$order_id/cancel" '{"reason":"smoke test"}'
check "200 OK" 200 "$status"
check "status is cancelled" cancelled "$(jq_ "d['status']")"
check "keyboard stock restored to 25" 25 "$(stock prd_keyboard)"
check "hub stock restored to 40" 40 "$(stock prd_hub)"
check "history ends with the stock released and the payment refunded" "OrderPlaced,StockReserved,PaymentCaptured,OrderCancelled,StockReleased,PaymentRefunded" "$(types "$order_id")"
check "the charge is now voided" voided "$(charge_status "$order_id")"

echo
echo "Cancelling again is refused"
req POST "$ORDER/orders/$order_id/cancel" ''
check "409 Conflict" 409 "$status"
check "problem type is invalid-state" urn:gocart:problem:invalid-state "$(jq_ "d['type']")"

echo
echo "Cart and checkout: a declined card keeps the cart, a retried checkout makes one order"
req GET "$CART/carts/cust_cart" ""
check "a new customer has an empty cart" 0 "$(jq_ "len(d['items'])")"
req PUT "$CART/carts/cust_cart/items/prd_nope" '{"quantity":1}'
check "an unknown product cannot be added (422)" 422 "$status"
req PUT "$CART/carts/cust_cart/items/prd_hub" '{"quantity":2}'
check "adding the hub is 200" 200 "$status"
rev=$(jq_ "d['revision']")
req PUT "$CART/carts/cust_cart/items/prd_hub" '{"quantity":2}'
check "setting the same quantity again does not change the cart" "$rev" "$(jq_ "d['revision']")"
check "the cart holds no price" False "$(python3 -c 'import sys; print("price" in sys.argv[1])' "$body")"
req POST "$CART/carts/cust_cart/checkout" '{"payment_token":"tok_declined"}'
check "a declined card is 402, passed through from the Order service" 402 "$status"
req GET "$CART/carts/cust_cart" ""
check "the cart is kept after a decline" 1 "$(jq_ "len(d['items'])")"
req POST "$CART/carts/cust_cart/checkout" '{"payment_token":"tok_visa"}'
check "checking out with another card is 201" 201 "$status"
cart_order=$(jq_ "d['id']")
check "the order is confirmed" confirmed "$(jq_ "d['status']")"
check "hub stock went down by the 2 ordered" 38 "$(stock prd_hub)"
req POST "$CART/carts/cust_cart/checkout" '{"payment_token":"tok_visa"}'
check "repeating the checkout is 200, not a second order" 200 "$status"
check "and it is the same order" "$cart_order" "$(jq_ "d['id']")"
check "hub stock did not drop again" 38 "$(stock prd_hub)"
req GET "$CART/carts/cust_cart" ""
check "the cart says which order it became" "$cart_order" "$(jq_ "d['order_id']")"

echo
echo "Shipping and delivering: in order, once, and then it is final"
req POST "$ORDER/orders/$cart_order/deliver" '' "${ADMIN[@]}"
check "delivering an order that has not shipped is 409" 409 "$status"
req POST "$ORDER/orders/$cart_order/ship" '' "${ADMIN[@]}"
check "shipping is 200" 200 "$status"
req POST "$ORDER/orders/$cart_order/deliver" '' "${ADMIN[@]}"
check "delivering is 200" 200 "$status"
check "status is delivered" delivered "$(jq_ "d['status']")"
req POST "$ORDER/orders/$cart_order/deliver" '' "${ADMIN[@]}"
check "delivering twice is 409" 409 "$status"
req POST "$ORDER/orders/$cart_order/cancel" ''
check "a delivered order cannot be cancelled (409)" 409 "$status"

echo
echo "One request id follows a request through both services"
req GET "$ORDER/orders/ord_does_not_exist" "" -H 'X-Request-ID: smoke-trace-1'
check "404 Not Found" 404 "$status"
check "request id is echoed in the problem" smoke-trace-1 "$(jq_ "d['request_id']")"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
