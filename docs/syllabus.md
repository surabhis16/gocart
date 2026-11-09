# Syllabus coverage

Where each topic of the Microservices Architecture syllabus (Units I to V) is covered in this project. Paths in backticks are relative to the repository root. Rows marked "(extra)" go beyond the syllabus.

| Unit | Topic | Where | What demonstrates it |
|---|---|---|---|
| I | What are microservices; reducing coordination costs | [architecture.md](architecture.md#what-makes-these-microservices) | separate modules, no shared code or data, one written-down contract |
| I | Team topologies and the operating model | [architecture.md](architecture.md#team-topology) | stream-aligned and platform teams, with interaction modes |
| I | SEED(S): actors, jobs, sequence diagrams | [seed.md](seed.md) | actors, 17 jobs, 4 sequence diagrams and a state machine |
| I | Actions and queries from jobs; open-standard specification | [openapi/](openapi) | OpenAPI 3.0.3 for the four back-end services, enforced by a test |
| I | Microservices versus APIs | [seed.md](seed.md#microservices-versus-apis) | API as contract, microservice as independently deployable unit |
| II | Service boundaries, domain-driven design, event storming | [architecture.md](architecture.md#boundaries) | four bounded contexts that each mean something different by "order" and "product" |
| II | Universal sizing formula | [architecture.md](architecture.md#sizing-the-services) | why these services, and the likely next split |
| II | Microservices embed their data | [architecture.md](architecture.md#data) | no shared database; a different store per service |
| II | **Event sourcing and CQRS** | `order-service/internal/{domain,eventstore,projection}` | replayable event log, optimistic concurrency, separate read model, old logs still replay after a change |
| II | Sagas and compensation (extra) | [architecture.md](architecture.md#orders-are-a-saga), [payments](architecture.md#payments) | decline versus no answer, idempotent charge, tombstone, expiry, refund |
| III | DevOps principles, IaC environment and pipeline | [operations.md](operations.md#infrastructure-as-code), `.github/workflows/` | CI that runs fmt, validate and plan; release by tag |
| III | Infrastructure components (AWS) | `infra/terraform/` | registries, log groups, least-privilege CI policy; plan of 16 resources |
| IV | Developer workspace and coding standards | [operations.md](operations.md#developer-workspace) | `gofmt`, `vet` and the race detector enforced in CI |
| IV | Containerised environment, Docker | `*/Dockerfile`, `docker-compose.yml` | two-stage builds into `scratch`, non-root, health checks |
| IV | **Cassandra** | `eventstore/cassandra.go`, `docker-compose.cassandra.yml`, `infra/k8s/cassandra/` | one event-store contract suite on both stores, two Order replicas on Cassandra |
| IV | **Kubernetes** | `infra/k8s/` | Kustomize base and overlays, hardened pods, NetworkPolicies |
| IV | Designing endpoints; second microservice; **umbrella project** | this repository | one repo, `go.work`, five independently built services |
| IV | API gateway and front end (extra) | [architecture.md](architecture.md#the-web-gateway-and-the-storefront), `web-service/` | allow-listed gateway, embedded React app, types generated from the OpenAPI files |
| IV | Identity, edge and observability (extra) | [architecture.md](architecture.md#who-is-asking-sign-in-and-ownership), [operations.md](operations.md#admin-key-and-metrics) | OIDC with PKCE through Dex, Traefik with HTTPS and rate limits, `/metrics` on every service |
| V | Staging environment, shipping and deploying the container, clean up | `scripts/ship.sh`, `scripts/cleanup.sh`, `release.yml` | registry flow into a `gocart-staging` namespace |
| V | Managing change | [operations.md](operations.md#managing-change) | additive, breaking and stored-data changes, each handled differently |
| V | Complexity and simplification; measuring progress | [operations.md](operations.md#measuring-progress) | delivery and coordination metrics, with measured recovery times |

Verification details for each part are in the table in the [README](../README.md#verification); the short future-scope table is at the end of [architecture.md](architecture.md#future-scope).
