# Syllabus coverage

Where each topic of the Microservices Architecture syllabus (Units I to V) is covered in this project. Paths in backticks are relative to the repository root. 21 topics; each row names the file to open. The two rows marked "(extra)" are not syllabus items but support them.

| Unit | Topic | Where | What demonstrates it |
|---|---|---|---|
| I | What are microservices; reducing coordination costs | [architecture.md](architecture.md#what-makes-these-microservices-unit-i) | separate modules, no shared code or data, one written-down contract |
| I | Team topologies | [architecture.md](architecture.md#team-topology) | stream-aligned and platform teams, with interaction modes |
| I | SEED(S): actors, jobs, sequence diagrams | [seed.md](seed.md) | 3 actors, 13 jobs, 3 sequence diagrams |
| I | Actions and queries from jobs; open-standard spec | [openapi/](openapi) | OpenAPI 3.0.3 for all three services, enforced by a test |
| I | Microservices versus APIs | [seed.md](seed.md#microservices-versus-apis) | |
| II | Service boundaries, DDD, event storming | [architecture.md](architecture.md#boundaries-unit-ii) | three bounded contexts that each mean something different by "order" and "product" |
| II | Universal sizing formula | [architecture.md](architecture.md#the-universal-sizing-formula) | why three services, and the likely first split |
| II | Microservices embed their data | [architecture.md](architecture.md#data-unit-ii) | no shared database; a different store per service |
| II | **Event sourcing and CQRS** | `order-service/internal/{domain,eventstore,projection}` | replayable event log, optimistic concurrency, separate read model, old logs still replay after a change |
| II | Sagas and compensation (extra) | [architecture.md](architecture.md#orders-are-a-saga), [payments](architecture.md#payments) | decline vs no-answer, idempotent charge, tombstone, expiry, refund |
| III | DevOps principles, IaC environment and pipeline | [operations.md](operations.md#infrastructure-as-code-unit-iii), `.github/workflows/` | CI that runs fmt, validate and plan |
| III | Infrastructure components (AWS) | `infra/terraform/` | registries, log groups, least-privilege CI policy |
| IV | Developer workspace, coding standards | [operations.md](operations.md#developer-workspace-unit-iv) | `gofmt`, `vet` and the race detector enforced in CI |
| IV | Containerised environment, Docker | `*/Dockerfile`, `docker-compose.yml` | two-stage builds into `scratch`, non-root, health checks |
| IV | Observability and access control (extra) | [operations.md](operations.md#admin-key-and-metrics) | `/metrics` on every service, admin endpoints behind a key |
| IV | **Cassandra** | `eventstore/cassandra.go`, `docker-compose.cassandra.yml`, `infra/k8s/cassandra/` | the same event-store contract on Cassandra (see the verification table in the [README](../README.md#what-is-verified-and-what-is-not)) |
| IV | **Kubernetes** | `infra/k8s/` | Kustomize base and three overlays, hardened pods |
| IV | Designing endpoints; second microservice; **umbrella project** | this repository | one repo, `go.work`, three independently built services |
| V | Staging environment, shipping and deploying the container, clean up | `scripts/ship.sh`, `scripts/cleanup.sh`, `release.yml` | registry flow into a `gocart-staging` namespace |
| V | Managing change | [operations.md](operations.md#managing-change-unit-v) | additive, breaking and stored-data changes, each handled differently; 0.1 to 0.2 is a worked example of the last two |
| V | Complexity and simplification; measuring progress | [operations.md](operations.md#measuring-progress-unit-v) | delivery and coordination metrics, with real numbers |

## Before you submit

Three things only you can supply:

1. **Step 6 of SEED asks for feedback on the API spec.** Nobody outside this project has reviewed it, and [seed.md](seed.md#6-getting-feedback-on-the-specification) says so plainly. Have a classmate try to place an order from `order.yaml` alone, then add what they found.
2. **Be ready to say the spec is authoritative, not that it came first.** A test fails the build if code and spec disagree, but nothing proves which was written first, and the docs do not claim it.
3. **Know the limits** in [architecture.md](architecture.md#known-limits): one replica each, only a shared admin key for access control, a mock payment provider, and a polling reconciler. They are stated so you can defend them, and an examiner asking "what would you change for production?" is asking you to read that table.
