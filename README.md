# GoCart

An e-commerce backend as two Go microservices, built to show what the Microservices Architecture syllabus is *for*: what happens at the seam between two services when the network, a crash or a double-click gets involved.

- **Product service** owns the catalogue and the stock that others reserve.
- **Order service** owns orders. It is event sourced: an order is the list of things that happened to it, and its state is whatever replaying them gives.

The part worth seeing: stop the Product service mid-checkout, and the customer still gets an honest answer (`202 Accepted`, `status: placed`), and the order **completes itself** when Product returns, with stock deducted exactly once.

```
docker compose up --build        # Product :8080, Order :8081
./scripts/smoke.sh               # 26 checks across both services
./scripts/outage-demo.sh         # the demo above, scripted
```

Both services have **no third-party dependencies**: the standard library only. That is a design choice, not an omission: nothing to download, nothing to patch, and a static binary of 6.8 MB (Product) and 6.9 MB (Order) in an empty `scratch` image, as the Dockerfiles build them.

![Architecture](docs/diagrams/context.png)

## Where the syllabus is covered

All five units, 19 topics. Each row names the file to open.

| Unit | Topic | Where | What demonstrates it |
|---|---|---|---|
| I | What are microservices; reducing coordination costs | [architecture.md](docs/architecture.md#what-makes-these-microservices-unit-i) | separate modules, no shared code or data, one written-down contract |
| I | Team topologies | [architecture.md](docs/architecture.md#team-topology) | stream-aligned and platform teams, with interaction modes |
| I | SEED(S): actors, jobs, sequence diagrams | [seed.md](docs/seed.md) | 3 actors, 9 jobs, 3 diagrams |
| I | Actions and queries from jobs; open-standard spec | [openapi/](docs/openapi) | OpenAPI 3.0.3 for both services, enforced by a test |
| I | Microservices versus APIs | [seed.md](docs/seed.md#microservices-versus-apis) | |
| II | Service boundaries, DDD, event storming | [architecture.md](docs/architecture.md#boundaries-unit-ii) | two bounded contexts with different meanings of "product" |
| II | Universal sizing formula | [architecture.md](docs/architecture.md#the-universal-sizing-formula) | why two services, and the likely first split |
| II | Microservices embed their data | [architecture.md](docs/architecture.md#data-unit-ii) | no shared database; a different store per service |
| II | **Event sourcing and CQRS** | `order-service/internal/{domain,eventstore,projection}` | replayable event log, optimistic concurrency, separate read model |
| III | DevOps principles, IaC environment and pipeline | [operations.md](docs/operations.md#infrastructure-as-code-unit-iii), `.github/workflows/` | CI that runs fmt, validate and plan |
| III | Infrastructure components (AWS) | `infra/terraform/` | registries, log groups, least-privilege CI policy |
| IV | Developer workspace, coding standards | [operations.md](docs/operations.md#developer-workspace-unit-iv) | `gofmt`, `vet` and the race detector enforced in CI |
| IV | Containerised environment, Docker | `*/Dockerfile`, `docker-compose.yml` | two-stage builds into `scratch`, non-root, health checks |
| IV | **Cassandra** | `eventstore/cassandra.go`, `docker-compose.cassandra.yml`, `infra/k8s/cassandra/` | the same event-store contract on Cassandra (see verification below) |
| IV | **Kubernetes** | `infra/k8s/` | Kustomize base and three overlays, hardened pods |
| IV | Designing endpoints; second microservice; **umbrella project** | this repository | one repo, `go.work`, two independently built services |
| V | Staging environment, shipping and deploying the container, clean up | `scripts/ship.sh`, `scripts/cleanup.sh`, `release.yml` | registry flow into a `gocart-staging` namespace |
| V | Managing change | [operations.md](docs/operations.md#managing-change-unit-v) | additive, breaking and stored-data changes, each handled differently |
| V | Complexity and simplification; measuring progress | [operations.md](docs/operations.md#measuring-progress-unit-v) | delivery and coordination metrics, with real numbers |

## Layout

```
gocart-umbrella/
├── product-service/          Go module: catalogue, stock, reservations
├── order-service/            Go module: orders, event log, read model, reconciler
├── infra/
│   ├── terraform/            AWS resources as code (plan runs offline, free)
│   └── k8s/                  Kustomize: base, cassandra, overlays {local, staging, staging-cassandra}
├── docs/
│   ├── seed.md  architecture.md  operations.md
│   ├── openapi/              product.yaml, order.yaml
│   └── diagrams/             Mermaid sources, rendered to SVG and PNG
├── scripts/                  smoke, outage-demo, ship, cleanup, bump-version, check-k8s
├── docker-compose.yml        + docker-compose.cassandra.yml
├── .github/workflows/        ci.yml, release.yml
└── Makefile                  `make help`
```

## What is verified, and what is not

I built this in a sandbox that could not download Go modules or pull container images, and had no Docker daemon, Kubernetes or Terraform. So the honest picture is:

| Part | Status |
|---|---|
| Both services: 76 tests (97 with subtests), `-race`, `gofmt`, `go vet` | **Passing.** Run here. |
| Both services running as real processes, driven by `smoke.sh` | **26 of 26 checks pass.** Run here. |
| Outage and recovery demo against the real processes | **Passes.** Run here. |
| Restart rebuilds the read model; clean SIGTERM shutdown; `-healthcheck` | **Checked** on the real binaries. |
| Tests that can fail | The OpenAPI drift check and key service behaviours were **mutation-tested**: I broke them on purpose and watched the tests go red. |
| Kubernetes manifests | **Schema-valid** against Kubernetes 1.31 (strict), with cross-reference checks. Never applied to a cluster. |
| Compose files | **Valid** (`docker compose config`). Never started. |
| Scripts | `shellcheck` clean. `bump-version.sh` **round-trip tested**. `ship.sh` and `cleanup.sh` **not run**. |
| Terraform | Parsed as HCL and cross-checked by script. **`validate` and `plan` not run.** |
| Dockerfiles | Not built. The Go they compile is the Go that was tested. |
| **Cassandra event store** | **Not compiled against the real driver, not run.** Type-checked against a stub only. |
| GitHub Actions workflows | Valid YAML. **Never run.** |

The CI pipeline is where the unverified parts get verified: it compiles the Cassandra store against the real driver and tests it on a live Cassandra, runs `terraform validate` and `plan`, builds both images, and runs the smoke test and outage demo against them. Push this to a GitHub repository and read the results before you rely on any of those rows. If one fails, the failure message will point at what to fix.

## Before you submit

Three things only you can supply:

1. **Step 6 of SEED asks for feedback on the API spec.** Nobody outside this project has reviewed it, and [seed.md](docs/seed.md#6-getting-feedback-on-the-specification) says so plainly. Have a classmate try to place an order from `order.yaml` alone, then add what they found.
2. **Be ready to say the spec is authoritative, not that it came first.** A test fails the build if code and spec disagree, but nothing proves which was written first, and the docs do not claim it.
3. **Know the limits** in [architecture.md](docs/architecture.md#known-limits): one replica each, no authentication, and a polling reconciler. They are stated so you can defend them, and an examiner asking "what would you change for production?" is asking you to read that table.
