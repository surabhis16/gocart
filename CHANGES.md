# Changes in 0.4.0 (from 0.3.0)

Adds a web storefront and admin console, served by a fifth service. Nothing in the four existing services changed. If you committed 0.3.0 to git first, `git diff` after unzipping shows exactly this.

## New
- **`web-service/`** (port 3000): a Go gateway (standard library only) that embeds the compiled React app and forwards an allow-list of `/api/*` routes to the other four services. Stock reservations and charge creation are not forwarded. Errors keep their problem format; an unreachable service is a `502` naming it. `GET /api/status` reports each service's health. A test ties every forwarded route to the OpenAPI files.
- **`web-service/frontend/`**: React 19, TypeScript, Vite, Tailwind, React Router, TanStack Query. Shop, cart with a test-payment-method picker (declined, no funds, expired, flaky, slow and down cards), orders, an order page set out as a receipt that updates itself while the order settles, cancel, and an admin console behind the admin key (orders with ship, deliver and cancel; an order inspector with the charge and the full event log; products and stock; service health). API types are generated from `docs/openapi/`. A guide for whoever restyles it is in `frontend/DESIGN.md`.
- Multi-stage `Dockerfile` (Node builds the app, Go embeds it, `scratch` runs it), compose (port 3000), a Kubernetes manifest with two replicas and a rolling update, a Terraform repository and log group, a `frontend` CI job (type-check, lint, tests, build, generated types up to date), release matrix, `ship.sh`, `cleanup.sh`, `bump-version.sh`, `check-k8s.py`, and `make web` / `make web-dev`.
- `smoke.sh` has 82 checks: ten new ones cover the gateway (app served, deep links, security header, internal routes refused, a cart read through it).

## Compatibility
- Everything is additive. Ports 8080 to 8083 and every endpoint are unchanged.
- Terraform gains one repository and one log group (the plan grows by the resources for the fifth service).

## Numbers
227 Go tests (276 with subtests, all with `-race`) and 20 front-end tests.

## Verified here, and not
Run here: all Go tests, the front-end tests, type-check, lint and production build; the real stack of five processes with the built app, driven in a real browser (browse, add to cart, pay, receipt, admin unlock, order inspector, system page); `smoke.sh` 82 of 82; `check-k8s.py`; `docker compose config`. Not run here: the Docker image build (the Node stage and the new CI job have not run), Terraform, and the Kubernetes deployment of the gateway.
