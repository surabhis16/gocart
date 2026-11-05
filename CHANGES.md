# Changes in 0.2.0 (from 0.1.0)

Added a real Payment service and the saga around it, plus access control and metrics. If you committed 0.1.0 to git first, `git diff` after unzipping shows exactly this.

## Breaking
- `POST /orders` now requires `payment_token` (for example `tok_visa`). Add that field to anything written against 0.1.
- `GET /orders/{id}/events`, `POST /orders/{id}/ship`, `POST /products`, `PUT /products/{id}/stock` and `GET /charges/{order_id}` need `Authorization: Bearer <ADMIN_KEY>`. Compose and the K8s overlays use the demo value `dev-admin-key`.
- Order statuses added: `awaiting_payment`, `payment_failed`, `expired`. A successful order now has three events (`OrderPlaced`, `StockReserved`, `PaymentCaptured`), not two.
- New failure response: `402` `payment-declined` on `POST /orders`.
- Logs written by 0.1 still replay. A 0.1 binary cannot read a log that 0.2 has written to.

## New
- `payment-service/` (port 8082): mock card network chosen by token (`tok_visa`, `tok_declined`, `tok_insufficient_funds`, `tok_expired_card`, `tok_flaky`, `tok_down`, `tok_slow`), idempotent one-charge-per-order ledger, void with tombstone.
- Order saga: reserve stock, charge, and give back what is owed (stock release, refund) on decline, expiry (`PAYMENT_TTL`, default 15m) and cancel. The two debts are tracked and retried independently.
- `/metrics` (Prometheus text format) on every service; admin-key auth with constant-time comparison; a service with no `ADMIN_KEY` refuses admin calls.
- `scripts/oversell-demo.sh`; `scripts/outage-demo.sh` now has two acts (Product down, Payment down); `scripts/smoke.sh` has 48 checks.
- `docs/openapi/payment.yaml`, `docs/diagrams/payment-failure.*`, `docs/syllabus.md` (the syllabus mapping moved out of the README).
- Compose, Kubernetes (with a generated admin Secret), Terraform (third repository and log group), CI and release matrices, `ship.sh`, `cleanup.sh`, `bump-version.sh`, `check-k8s.py` all cover the third service.

## Numbers
164 tests (211 with subtests), up from 76 (97). Static binaries about 6.5 MB each.

## Not verified here (unchanged from 0.1)
No Docker daemon, Terraform, Kubernetes cluster or live Cassandra was available. See the verification table in the README; CI is where those get checked.
