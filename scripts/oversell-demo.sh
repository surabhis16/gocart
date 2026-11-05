#!/usr/bin/env bash
# Many customers, little stock: does the system sell more than it has?
#
#   ./scripts/oversell-demo.sh                  # against docker compose
#
# Twenty customers each try to buy one lamp at the same moment, and each one
# double-clicks (the identical request is sent twice at once). There are only
# three lamps. A correct system confirms exactly three orders, rejects the
# rest, ends with no stock, takes money for exactly three lamps, and never
# charges anyone twice for a double-click.
#
# Changes stock, so it sets the lamp stock itself; it is safe to run repeatedly.
set -euo pipefail

PRODUCT="${PRODUCT:-http://localhost:8080}"
ORDER="${ORDER:-http://localhost:8081}"
PAYMENT="${PAYMENT:-http://localhost:8082}"
ADMIN_KEY="${ADMIN_KEY:-dev-admin-key}"
BUYERS="${BUYERS:-20}"
STOCK="${STOCK:-3}"

for url in "$PRODUCT" "$ORDER" "$PAYMENT"; do
  for _ in $(seq 1 60); do curl -fsS "$url/healthz" >/dev/null 2>&1 && break; sleep 1; done
  curl -fsS "$url/healthz" >/dev/null || { echo "$url is not healthy" >&2; exit 1; }
done

exec python3 - "$PRODUCT" "$ORDER" "$PAYMENT" "$ADMIN_KEY" "$BUYERS" "$STOCK" <<'PY'
import json, sys, threading, time, urllib.request, urllib.error, uuid

product, order, payment, admin_key, buyers, stock = sys.argv[1:4] + [sys.argv[4]] + [int(sys.argv[5]), int(sys.argv[6])]
run = uuid.uuid4().hex[:8]  # makes idempotency keys unique per run

def call(method, url, body=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": "application/json", **(headers or {})}
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"null")

admin = {"Authorization": "Bearer " + admin_key}
def say(msg): print("\n\033[1m%s\033[0m" % msg)

say("Set lamp stock to %d" % stock)
code, _ = call("PUT", product + "/products/prd_lamp/stock", {"stock": stock}, admin)
assert code == 200, "could not set stock (status %s); is ADMIN_KEY right?" % code

say("%d customers x 2 identical requests each, all at once" % buyers)
results = []      # (customer, status, body)
lock = threading.Lock()
barrier = threading.Barrier(buyers * 2)

def buy(i):
    barrier.wait()
    body = {"customer_id": "cust_%s_%d" % (run, i), "payment_token": "tok_visa",
            "items": [{"product_id": "prd_lamp", "quantity": 1}]}
    code, resp = call("POST", order + "/orders", body, {"Idempotency-Key": "oversell-%s-%d" % (run, i)})
    with lock:
        results.append((i, code, resp))

threads = [threading.Thread(target=buy, args=(i,)) for i in range(buyers) for _ in range(2)]
t0 = time.time()
for t in threads: t.start()
for t in threads: t.join()
print("   %d requests finished in %.2fs" % (len(results), time.time() - t0))

by_customer = {}
for i, code, resp in results:
    by_customer.setdefault(i, []).append((code, resp))

confirmed_ids, rejected_ids = set(), set()
for i, outcomes in by_customer.items():
    ids = set()
    for code, resp in outcomes:
        assert code in (200, 201, 409), "unexpected status %s: %s" % (code, resp)
        ids.add(resp["id"] if code != 409 else resp["order_id"])
    assert len(ids) == 1, "customer %d got two different orders from one idempotency key: %s" % (i, ids)
    oid = ids.pop()
    if any(code in (200, 201) for code, _ in outcomes):
        confirmed_ids.add(oid)
    else:
        rejected_ids.add(oid)

left = call("GET", product + "/products/prd_lamp")[1]["stock"]
charges = [oid for oid in confirmed_ids if call("GET", payment + "/charges/" + oid, headers=admin)[1].get("status") == "approved"]
stray = [oid for oid in rejected_ids if call("GET", payment + "/charges/" + oid, headers=admin)[0] != 404]

say("Result")
print("   customers: %d   confirmed orders: %d   rejected orders: %d" % (buyers, len(confirmed_ids), len(rejected_ids)))
print("   lamp stock left: %d   approved charges: %d   charges for rejected orders: %d" % (left, len(charges), len(stray)))

expect = min(stock, buyers)
problems = []
if len(confirmed_ids) != expect: problems.append("confirmed %d orders, expected exactly %d" % (len(confirmed_ids), expect))
if len(rejected_ids) != buyers - expect: problems.append("rejected %d, expected %d" % (len(rejected_ids), buyers - expect))
if left != stock - expect: problems.append("stock left is %d, expected %d" % (left, stock - expect))
if len(charges) != expect: problems.append("%d approved charges, expected %d" % (len(charges), expect))
if stray: problems.append("%d rejected orders were charged" % len(stray))
if problems:
    print("\n\033[31mFAILED\033[0m"); [print("   - " + p) for p in problems]; sys.exit(1)
say("Done: exactly %d sold, nothing oversold, nobody charged twice." % expect)
PY
