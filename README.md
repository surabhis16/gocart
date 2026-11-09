# GoCart

An e-commerce system built as five Go microservices behind a React storefront, with real OpenID Connect sign-in, an event-sourced order service, a saga that survives outages, and a Kubernetes and Terraform delivery path. It is designed around what happens at the seam between services when the network, a crash, a declined card or a double-click gets involved.

## The system

- **Product service** owns the catalogue and the stock that others reserve.
- **Order service** owns orders. It is event sourced: an order is the list of things that happened to it, and its state is whatever replaying them gives. It runs the saga: reserve stock, take payment, and give everything back if the order does not complete. Its events live in a file or in Cassandra, and two replicas stay consistent through a change feed.
- **Cart service** owns what a customer intends to buy. Its checkout turns a cart into exactly one order, however many times the button is pressed or the network drops. A cart holds no prices, because the Product service owns those.
- **Payment service** owns charges. A mock card network approves, declines or fails depending on the payment token, so the hard cases can be produced on demand. Card data is never stored.
- **Web service** is the gateway. It serves the React storefront and the admin console from one binary, does the OpenID Connect sign-in, and is the only door from a browser to the four services above, forwarding a short allow-list of routes.

## What stands out

- **Resilience.** Stop the Product or the Payment service mid-checkout and the customer still gets an honest answer (`202 Accepted`); the order completes itself when the service returns, with stock reserved once and money taken once. A declined card is a definite answer (`402`) and the stock goes straight back.
- **Correctness under concurrency.** 20 customers each double-click "buy" at the same instant for 3 lamps: exactly 3 are sold, 3 are charged, nobody is charged twice.
- **Event sourcing and CQRS.** An append-only log with optimistic concurrency, a separate read model rebuilt by replay, and old logs that keep replaying after the schema changes.
- **Identity done properly.** Authorization-code flow with PKCE through Dex (which fronts Google and GitHub), RS256 token verification, signed stateless sessions, CSRF protection, and a short-lived signed identity the gateway passes on so the Order and Cart services enforce ownership themselves.
- **Contracts.** OpenAPI 3.0.3 for every service, enforced by a route-drift test; RFC 9457 problem documents; front-end types generated from the specs.
- **Production-shaped delivery.** Hardened pods, default-deny NetworkPolicies, a Traefik edge with cert-manager HTTPS and rate limits, Kustomize overlays, Terraform for the registry, logs and CI permissions, and GitHub Actions for test, release and end-to-end runs.
- **Observability.** Structured JSON logs, a request id carried across all five services, and Prometheus metrics on each.
- **Dependency-free back end.** The five Go services use the standard library only: nothing to download, nothing to patch, and a static binary of about 6.5 MB in an empty `scratch` image.

## Tech stack

| Layer | Technology |
|---|---|
| Services | Go 1.22 (standard library only), `go.work` workspace |
| Storage | Atomic JSON snapshots, append-only event log on file or Apache Cassandra |
| Front end | React 19, TypeScript, Vite 6, Tailwind, TanStack Query, React Router, Vitest |
| Identity | OpenID Connect, PKCE, Dex, Google and GitHub |
| Containers | Docker multi-stage builds, Docker Compose |
| Orchestration | Kubernetes, Kustomize, Traefik, cert-manager |
| Infrastructure as code | Terraform (or OpenTofu), AWS ECR, CloudWatch, IAM |
| CI/CD | GitHub Actions, `ghcr.io` |

## Quick start

```
docker compose up --build        # open http://localhost:3000 and press Sign in (accounts and Google/GitHub setup: docs/operations.md)
                                 # Product :8080, Order :8081, Payment :8082, Cart :8083
./scripts/smoke.sh               # 99 checks across all five services
./scripts/oversell-demo.sh       # many buyers, little stock
./scripts/outage-demo.sh         # Product down, then Payment down
```

![Architecture](docs/diagrams/context.png)

## Layout

```
gocart-umbrella/
├── product-service/          Go module: catalogue, stock, reservations
├── order-service/            Go module: orders, event log, read model, saga, reconciler
├── payment-service/          Go module: mock card network, charge ledger
├── cart-service/             Go module: carts, retry-safe checkout
├── web-service/              Go gateway (OIDC, sessions) + frontend/ (React, TypeScript, Vite, Tailwind); see frontend/DESIGN.md
├── infra/
│   ├── terraform/            AWS resources as code (plan runs offline, free)
│   └── k8s/                  Kustomize
│       ├── base/             services, NetworkPolicies, Dex (config and GoCart theme)
│       ├── cassandra/        one-node StatefulSet
│       ├── edge/             Traefik routes, rate limits, certificate
│       ├── cert-manager/     issuers (self-signed, Let's Encrypt example)
│       └── overlays/         local, staging, staging-cassandra, local-https, local-https-private.example
├── docs/
│   ├── architecture.md       how it works and why
│   ├── operations.md         run, deploy, change, observe
│   ├── seed.md               design process: actors, jobs, diagrams, APIs
│   ├── syllabus.md           mapping to the course syllabus
│   ├── openapi/              product.yaml, order.yaml, payment.yaml, cart.yaml
│   └── diagrams/             Mermaid sources, rendered to SVG and PNG
├── scripts/                  smoke, oversell-demo, outage-demo, ship, cleanup, bump-version, check-k8s, mint
├── docker-compose.yml        + docker-compose.cassandra.yml
├── .env.example              sign-in settings for Compose
├── .github/workflows/        ci.yml, release.yml
└── Makefile                  `make help`
```

## Verification

| Area | Verified |
|---|---|
| Go back end | 279 tests (331 with subtests) across five modules under `-race`, with `gofmt` and `go vet` clean |
| Front end | 25 tests, type-check, lint, production build, generated API types up to date |
| Whole system | `smoke.sh`: 99 checks on the five services running together, covering orders, payments, carts, ownership between customers, the gateway's sessions and CSRF, admin gating and metrics |
| Concurrency and failure | Oversell demo (exactly 3 sold, 3 charged); outage demo (order completes by itself: `StockReserved` 10.8 s after `OrderPlaced`, `PaymentCaptured` 10.9 s after `StockReserved`, at a 1 s reconcile interval and 10 s grace); checkout while the Order service is down, then recovered, gives one order |
| Browser flows | Storefront (browse, cart, pay, receipt, admin, system page, phone width), and sign-in with two customers unable to see each other's cart or orders, the admin gate, sign-out |
| Sign-in on Kubernetes | The full flow through Traefik and Dex with demo accounts, Google and GitHub |
| Event store | One contract suite passes on the file store and on live Cassandra in CI; Cassandra runs on Kubernetes (`staging-cassandra`) with two Order replicas on the change feed |
| Restart and shutdown | Restart rebuilds the read model (10,000 orders replay in about 0.17 s, 100,000 in about 2.0 s); clean SIGTERM shutdown; `-healthcheck` on every binary |
| Test quality | The OpenAPI drift check and key service behaviours were mutation-tested: broken on purpose, tests went red |
| Containers | Five images built and started with Docker Compose |
| Kubernetes manifests | 27 objects schema-validated against Kubernetes 1.31 (strict) and cross-checked by `check-k8s.py` (callers admitted by policies, one public address everywhere, Dex settings and theme); applied in the Cassandra staging overlay and the HTTPS overlay |
| Terraform | `validate` passes; `plan` shows 16 to add (5 repositories, 5 lifecycle policies, 5 log groups, 1 IAM policy) |
| Scripts | `shellcheck` clean; `bump-version.sh` round-trips; `ship.sh` runs the Cassandra staging flow |
| CI | GitHub Actions green: test matrix, Cassandra, front end, infrastructure and end-to-end jobs |

## Documentation

- [docs/architecture.md](docs/architecture.md): services, data, event sourcing, saga, sign-in, and the future-scope table.
- [docs/operations.md](docs/operations.md): run with Compose or Kubernetes, sign-in setup, Terraform, releasing, managing change, metrics.
- [docs/seed.md](docs/seed.md): the design process from actors to OpenAPI specifications.
- [docs/syllabus.md](docs/syllabus.md): where each course topic is covered.
