# Changes in 0.3.0 (from 0.2.0)

Adds a Cart service with a retry-safe checkout, an order delivery step, and lets the Order service run more than one replica on Cassandra. If you committed 0.2.0 to git first, `git diff` after unzipping shows exactly this.

## New
- **`cart-service/`** (port 8083): `GET /carts/{customer_id}`, `PUT` and `DELETE /carts/{customer_id}/items/{product_id}`, `DELETE /carts/{customer_id}`, `POST /carts/{customer_id}/checkout`. A cart holds no prices. Checkout sends the Order service an `Idempotency-Key` built from the cart's revision, so a retried checkout cannot create a second order; a declined card moves the cart to a new revision so another card can be tried. Contract: `docs/openapi/cart.yaml`. Diagram: `docs/diagrams/checkout.*`.
- **`POST /orders/{id}/deliver`** (admin key): only a `shipped` order can be delivered, and `delivered` is final (a delivered order cannot be cancelled). New status `delivered`, new event `OrderDelivered`.
- **Order replicas share a change feed.** Appending an event also records the order's id in an `order_changes` table (Cassandra); each replica polls it (`SYNC_INTERVAL`, default 2s) and reloads the whole log now and then (`RESYNC_INTERVAL`, default 5m) as a backstop. Commands and `GET /orders/{id}` are never stale; a list can trail by up to `SYNC_INTERVAL`. The `staging-cassandra` overlay now runs two Order replicas with a rolling update.
- Compose, Kubernetes, Terraform (fourth repository and log group), CI and release matrices, `ship.sh`, `cleanup.sh`, `bump-version.sh` and `check-k8s.py` all cover the fourth service. `smoke.sh` now has 71 checks (cart, checkout, ship then deliver).

## Compatibility
- Everything is additive. No existing endpoint changed.
- A log that contains `OrderDelivered` cannot be read by a 0.2 binary (replay refuses unknown event types, on purpose). Roll forward, not back, once an order has been delivered.
- The file store needs no change. A Cassandra keyspace gains the `order_changes` table, which the service creates itself on start.

## Numbers
213 tests (262 with subtests), up from 164 (211). All run with `-race`.