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
docker compose up --build        # open http://localhost:3000 (admin key: dev-admin-key)
                                 # Product :8080, Order :8081, Payment :8082, Cart :8083
./scripts/smoke.sh               # 82 checks across all five services
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

I built this in a sandbox that could not download Go modules or pull container images, and had no Docker daemon, Kubernetes or Terraform. So the honest picture is:

| Part | Status |
|---|---|
| All four services: 213 tests (262 with subtests), `-race`, `gofmt`, `go vet` | **Passing.** Run here. |
| The four services running as real processes, driven by `smoke.sh` | **72 of 72 checks pass.** Run here. |
| Checkout while the Order service is down, then recovered | **Checked on real processes**: `503`, cart unchanged; after restart one order, and a repeat returns it again. Run here. |
| Oversell demo against the real processes (run twice) | **Passes**: 3 sold, 3 charged, 0 stock left, no double charges. Run here. |
| Outage and recovery demo, both acts, against the real processes | **Passes.** Run here. |
| Restart rebuilds the read model; clean SIGTERM shutdown; `-healthcheck` | **Checked** on the real binaries. |
| Tests that can fail | The OpenAPI drift check and key service behaviours were **mutation-tested**: I broke them on purpose and watched the tests go red. |
| Kubernetes manifests | **Schema-valid** against Kubernetes 1.31 (strict), with cross-reference checks. Never applied to a cluster. |
| Compose files | **Valid** (`docker compose config`, both the base and the Cassandra overlay). Never started. |
| Scripts | `shellcheck` clean. `bump-version.sh` **round-trip tested**. `ship.sh` and `cleanup.sh` **not run**. |
| Terraform | Parsed as HCL and cross-checked by script. **`validate` and `plan` not run.** |
| Dockerfiles | Not built (no Docker daemon here). The Go they compile is the Go that was tested; the payment Dockerfile is the product one with the name and port changed. |
| **Cassandra event store** | **Not compiled against the real driver, not run.** Type-checked against a stub only. |
| GitHub Actions workflows | Valid YAML. **Never run.** |

The CI pipeline is where the unverified parts get verified: it compiles the Cassandra store against the real driver and tests it on a live Cassandra, runs `terraform validate` and `plan`, builds the four images, and runs the smoke test, the oversell demo and the outage demo against them. Push this to a GitHub repository and read the results before you rely on any of those rows. If one fails, the failure message will point at what to fix.

## Limits, and upgrading

The known limits are in a table in [architecture.md](docs/architecture.md#known-limits): one replica each (Order can run two on Cassandra), only a shared admin key for access control, a mock payment provider, and a polling reconciler. They are written down so they are choices you can defend, and the table is also the answer to "what would you change for production?".

Version 0.2 made `payment_token` **required** on `POST /orders`, a deliberate breaking change explained in [operations.md](docs/operations.md#a-real-breaking-change-and-how-it-was-handled). Anything written against 0.1 (a Postman collection, say) needs that one field added. Order logs written by 0.1 still replay.
