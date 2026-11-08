# GoCart

An e-commerce system as four Go microservices behind a React storefront, built to show what happens at the seam between services when the network, a crash, a declined card or a double-click gets involved.

- **Product service** owns the catalogue and the stock that others reserve.
- **Order service** owns orders. It is event sourced: an order is the list of things that happened to it, and its state is whatever replaying them gives. It runs the saga: reserve stock, take payment, and give everything back if the order does not complete.
- **Cart service** owns what a customer intends to buy. Its checkout turns a cart into exactly one order, however many times the button is pressed or the network drops. A cart holds no prices, because the Product service owns those.
- **Web service** serves the storefront and the admin console, and is the only door from a browser to the four services above. It forwards a short allow-list of routes and nothing else.
- **Payment service** owns charges. There is no real money: a mock card network approves, declines or fails depending on the payment token, so the hard cases can be produced on demand.

The parts worth seeing:

- Stop the Product or the Payment service mid-checkout and the customer still gets an honest answer (`202 Accepted`); the order **completes itself** when the service returns, with stock reserved once and money taken once.
- A declined card is a definite answer (`402`) and the stock goes straight back. A payment provider that *does not answer* is a different thing, and the system does not guess.
- 20 customers each double-click "buy" at the same instant for 3 lamps: exactly 3 are sold, 3 are charged, nobody is charged twice.

```
docker compose up --build        # open http://localhost:3000, press Sign in (demo accounts: see docs/operations.md; Google and GitHub sign-in via .env)
                                 # Product :8080, Order :8081, Payment :8082, Cart :8083
./scripts/smoke.sh               # 99 checks across all five services
./scripts/oversell-demo.sh       # many buyers, little stock
./scripts/outage-demo.sh         # Product down, then Payment down
```

The four back-end services and the gateway have **no third-party Go dependencies**: the standard library only. (The front end is the exception, and is built in a Docker stage so none of it reaches the final image.) That is a design choice, not an omission: nothing to download, nothing to patch, and a static binary of about 6.5 MB each in an empty `scratch` image, as the Dockerfiles build them.

![Architecture](docs/diagrams/context.png)

## Layout

```
gocart-umbrella/
├── product-service/          Go module: catalogue, stock, reservations
├── order-service/            Go module: orders, event log, read model, saga, reconciler
├── payment-service/          Go module: mock card network, charge ledger
├── cart-service/             Go module: carts, retry-safe checkout
├── web-service/              Go gateway + frontend/ (React, TypeScript, Vite, Tailwind); see frontend/DESIGN.md
├── infra/
│   ├── terraform/            AWS resources as code (plan runs offline, free)
│   └── k8s/                  Kustomize: base, cassandra, overlays {local, staging, staging-cassandra}
├── docs/
│   ├── seed.md  architecture.md  operations.md   (design process, how it works, how to run and change it)
│   ├── openapi/              product.yaml, order.yaml, payment.yaml, cart.yaml
│   └── diagrams/             Mermaid sources, rendered to SVG and PNG
├── scripts/                  smoke, oversell-demo, outage-demo, ship, cleanup, bump-version, check-k8s
├── docker-compose.yml        + docker-compose.cassandra.yml
├── .github/workflows/        ci.yml, release.yml
└── Makefile                  `make help`
```

## What is verified, and what is not

| Part | Status |
|---|---|
| Go: 279 tests (331 with subtests) across five modules, `-race`, `gofmt`, `go vet` | **Passing**, in the sandbox and in CI. |
| Front end: 25 tests, type-check, lint, production build, generated API types up to date | **Passing**|
| The five services running together, driven by `smoke.sh` | **99 of 99 checks pass** on real processes |
| The storefront in a real browser (browse, cart, pay, receipt, admin, system page, phone width) | **Working** |
| Sign-in and ownership in a real browser | **Working**: landing page, sign-in, two customers unable to see each other's cart or orders, the admin gate, sign-out, phone width. |
| Dex, Traefik, cert-manager, NetworkPolicies, Google and GitHub sign-in | **Working*". The manifests are schema-valid and cross-checked by `check-k8s.py` (including that every caller is admitted by a policy and the public address agrees everywhere), but need a real cluster. See [operations.md](docs/operations.md#sign-in-secrets-and-the-edge). |
| Checkout while the Order service is down, then recovered | **Checked on real processes**: `503`, cart unchanged; after restart one order, and a repeat returns it again. |
| Oversell demo and outage-and-recovery demo | **Pass** against the real processes, and in the CI `end-to-end` job. |
| Restart rebuilds the read model; clean SIGTERM shutdown; `-healthcheck` | **Checked** on the real binaries. |
| Tests that can fail | The OpenAPI drift check and key service behaviours were **mutation-tested**: broken on purpose, tests went red. |
| Dockerfiles and Compose | **Built and started** by the author (`docker compose up --build`, five images). |
| **Cassandra event store**, including two Order replicas on the change feed | **Passes** its tests on a live Cassandra in CI, and runs on Kubernetes (`staging-cassandra`). |
| Kubernetes manifests | **Schema-valid** against Kubernetes 1.31 (strict) with cross-reference checks, and **applied** by the author in the Cassandra staging overlay. The other overlays and the web gateway's two-replica rollout were not separately exercised. |
| Terraform | **`validate` passes; `plan` shows 16 to add** (5 repositories, 5 lifecycle policies, 5 log groups, 1 IAM policy). Never applied to a real AWS account. |
| Scripts | `shellcheck` clean. `bump-version.sh` round-trip tested. `ship.sh` run for the Cassandra staging flow; `cleanup.sh` not separately confirmed. |
| GitHub Actions workflows | **A full run is green**, including the Cassandra job. |

