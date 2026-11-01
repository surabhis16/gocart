# Architecture

![GoCart context](diagrams/context.png)

Two services, each in its own bounded context, each owning its own data. They meet at exactly one place: the Order service calls `POST /reservations` and `DELETE /reservations/{order_id}` on the Product service.

## What makes these microservices (Unit I)

A service earns the name by being changeable and deployable without asking anyone else. In GoCart that is concrete:

- **Separate modules.** `product-service/` and `order-service/` have their own `go.mod`, their own Dockerfile and their own tests. Neither imports the other. Each builds and tests from its own directory, and CI runs them as separate matrix jobs.
- **No shared code.** The request-ID and problem-document helpers are about a hundred lines and appear in both services. That duplication is deliberate: a shared library would mean a change to it forces both services to release together, which is exactly the coordination cost the architecture exists to avoid. Duplicating a hundred stable lines is cheaper than that coupling.
- **No shared data.** See [Data](#data-unit-ii).
- **One coordination point, written down.** The Order service's whole understanding of the Product service lives in one file, `order-service/internal/productclient/client.go`, and the contract it relies on is [`openapi/product.yaml`](openapi/product.yaml).

**Reducing coordination costs** is the argument behind all of that. Coordination between teams is the thing that does not scale: every meeting to agree a change, every release train, every "can you deploy before us". The design goal is to shrink what two teams must agree on to a small, explicit, versioned contract.

### Team topology

Using the four team types and three interaction modes from *Team Topologies* (Skelton and Pais):

| Team | Type | Owns | Interacts with |
|---|---|---|---|
| **Checkout** | stream-aligned | Order service, its event log and read model | Catalog via **X-as-a-Service** (the Product API) |
| **Catalog** | stream-aligned | Product service and its data | Checkout via the same API, which it treats as a product with consumers |
| **Platform** | platform | pipeline, Terraform, Kubernetes manifests, the container registry | both, via **X-as-a-Service** (a paved road, not tickets) |

Two additions in practice. For the first few weeks, while the reservation contract is still being found, Checkout and Catalog would use **collaboration** mode, working side by side; once it settles it hardens into X-as-a-Service, and that is the moment to freeze `openapi/product.yaml` behind versioning. And Platform's job is to make the right thing the easy thing: `scripts/ship.sh` and the CI workflows are that, in miniature.

This project has one author, so one person wears all three hats. The point of writing the topology down is that the *code* is already split along these lines: if three people did take it over tomorrow, nobody would need to change the architecture first.

## Boundaries (Unit II)

### Domain-driven design

The two bounded contexts are not just folders. Each has its own language, and the same real-world thing means something different in each:

| Concept | In the Product context | In the Order context |
|---|---|---|
| Product | A catalogue entry with a live price and a stock level. | Only an ID and a *snapshot*: the name and price quoted when stock was reserved. |
| Customer | Does not exist. | A string ID on an order. There is no customer context yet because nothing needs one. |
| Reservation | The central object: stock held for an order. | Never stored. The order records the *outcome* (confirmed, with these prices). |

The snapshot row is the one to dwell on. If the Order service stored a product's current price and looked it up later, a price change would silently rewrite an old order. Instead the Product service returns name and price *at the moment of reservation*, and the order keeps that copy. Two contexts hold "the price", and that duplication is correct: they are answering different questions (what does it cost now / what did this customer agree to pay).

### Event storming

Event storming starts from what *happens*, in past tense, on sticky notes, and works backwards to commands and the things that decide them. The outcome for GoCart:

| Kind | Items |
|---|---|
| **Domain events** (orange) | OrderPlaced, OrderConfirmed, OrderRejected, OrderCancelled, StockReleased, OrderShipped |
| **Commands** (blue) | Place order, Cancel order, Ship order |
| **Aggregate** (yellow) | Order (decides every command above; its stream of events is its state) |
| **Policies** (lilac): "whenever X, then Y" | whenever OrderPlaced, reserve stock · whenever OrderCancelled, release stock |
| **External system** (pink) | Product service |
| **Hot spots** (red): the questions the session raised | What does the customer see if Product is down? What if they click twice? What if the price changes after they browse? |

The three hot spots each became a design decision: the `202 Accepted` outcome and the reconciler, the `Idempotency-Key`, and the price snapshot. The two policies are the saga described below. They are the only places the Order service does something *because of* an event rather than because a user asked.

### The universal sizing formula

*Microservices: Up and Running* (Mitra and Nadareishvili) offers three principles in place of a numeric formula for "how big should a service be":

1. **Start with just a few microservices**, using bounded contexts as the starting boundaries.
2. **Keep splitting** as the system and its teams grow, guided by where coordination is hurting.
3. **Be on the right trajectory** for decreasing coordination. That matters far more than whether today's sizing is perfect.

Applied here: two services is principle 1, one per bounded context, and no smaller. The case for *not* splitting further is concrete. Stock and reservations live in the Product service together because reserving is one atomic step: check every line, deduct, and record the reservation, or do none of it. Splitting "Inventory" from "Reservations" would turn that into a distributed transaction, which is the problem the design is avoiding.

The likeliest first split, principle 2, would be **Catalog** (names, descriptions, prices: read-heavy, edited by merchandisers) away from **Inventory** (stock and reservations: write-heavy, changes every order). They change at different speeds and for different reasons, so they would eventually want different owners and release rhythms. The trigger is not "it feels big", it is "two teams keep waiting on each other to change this service". Until then, splitting is cost without benefit, and the splitting direction is easier to reverse than merging.

## Data (Unit II)

### Independent deployability and data sharing

There is no database that both services can reach, and that is a design rule, not an accident. A shared database would make every schema change a cross-team negotiation, and the services would no longer be independently deployable even though they run in separate containers. **Microservices embed their data**: each service's storage is private to it.

| | Product | Order |
|---|---|---|
| Stores | the *current state* of the catalogue and reservations | the *history* of every order |
| How | one JSON snapshot, replaced atomically on every change (write temp file, fsync, rename) | an append-only event log, fsynced on every append |
| Why that shape | a catalogue is state: nobody asks what the lamp's price was on a given Tuesday | an order is a story: what happened, in what order, matters |
| Recovery | crash-safe by construction: a reader sees the old file or the new, never half | a torn final write is detected and cut off at startup; corruption anywhere else refuses to start |

Choosing different storage for each service is the point: each picks what suits it. Cassandra, in the syllabus's Unit IV, suits the Order service's event log (append-heavy, partitioned by order), and the Order service can use it. See [operations.md](operations.md#cassandra).

### Event sourcing and CQRS

**Event sourcing.** An order is never stored as a row that gets updated. It is stored as the list of events that happened to it, and its state is whatever you get by replaying them. `GET /orders/{id}/events` shows that list for any order:

```
v1  OrderPlaced
v2  OrderConfirmed
v3  OrderCancelled
v4  StockReleased
```

What this buys, and where in the code:

- **Decisions are pure.** `internal/domain/order.go` holds the whole state machine as functions from state to events. It is tested as a table (`TestTransitionMatrix`: every command in every state) with no database, network or clock.
- **Concurrency without locks.** Every event carries its position in the order's stream (`version`). Appending version *n* succeeds only if the stream currently has *n-1* events; otherwise the store says `ErrVersionConflict` and the command reloads and decides again. Twelve simultaneous cancels of one order produce exactly one cancellation and eleven refusals (`TestConcurrentCancelsOnlyOneWins`).
- **Damaged history is detected, not guessed at.** Replay refuses gaps, duplicates, unknown event types and impossible transitions. Skipping a bad event would silently change what an order *is*.
- **The read model is disposable.** `TestRestartRebuildsEveryOrderFromTheLog` throws it away, restarts on the same log, and checks every order comes back exactly as it was.

**CQRS** (Command Query Responsibility Segregation) means the model that *decides* and the model that *answers* are different things:

- **Command side:** `POST /orders`, `.../cancel`, `.../ship`. These load the order's events, replay them, decide, and append. They never read the projection.
- **Query side:** `GET /orders`, `GET /orders/{id}`. These read `internal/projection`, a flat in-memory view with a by-customer index, shaped for the API instead of for decisions. They never replay events.

*An honest qualifier:* here both sides live in one process, and the view is updated immediately after each append, under a lock. That is CQRS with separate models, not with separate infrastructure, and it is not eventually consistent. What does hold is the separation that matters: commands decide from the log, queries read the view, and the view can be rebuilt at any time. Putting the view in another process, fed by a change feed, would be a transport change, not a redesign. See the scaling limits below.

### Event sourcing and CQRS beyond microservices

Neither idea needs microservices. A bank account is an event-sourced thing: the balance is derived from the ledger of transactions, never the other way round, and the statement is a read model. Version control is the same shape: a commit history is the log, a checkout is a projection. Event sourcing earns its keep wherever *how you got here* matters (audit, undo, dispute, debugging), and CQRS wherever reads and writes want different shapes. It is wasted on a plain CRUD table, which is why the Product service does not use it.

## Orders are a small saga

The two services cannot share a transaction, so placing an order is three steps with a crash possible between any two:

1. store `OrderPlaced`: the order exists, durably;
2. ask the Product service to reserve stock;
3. store `OrderConfirmed` (or `OrderRejected` if Product said no).

If anything stops between 1 and 3 (a timeout, an outage, a crash), the order is left `placed`. That is a real, queryable state, not a corrupt one. **The reconciler** (`Service.Reconcile`) finds orders that are still placed once they are older than a grace period, and finishes them. Because step 2 is idempotent per order ID on the Product side, finishing is always safe: asking again returns the existing reservation instead of reserving twice.

Cancelling is the same shape backwards: `OrderCancelled` is stored first, the stock release follows, and `StockReleased` is stored once it lands. Until then the order reports `stock_release_pending: true`, and the reconciler keeps trying.

This is demonstrated, not just described: `scripts/outage-demo.sh` stops the Product service, places an order (gets `202`), starts it again, and watches the order confirm itself, with stock deducted exactly once. In my run, with the reconciler polling every second, the event log shows `OrderConfirmed` recorded 10.2 seconds after `OrderPlaced`, which is the 10-second grace period plus a fraction of a tick. At the default 5-second interval, expect up to about 15 seconds.

Retries are safe on both sides of the call:

- **Customer to Order:** the `Idempotency-Key` header. The same key and request returns the same order (`200`), a different request under the same key is `422`. The key is scoped to the customer, so two customers using the same key do not collide.
- **Order to Product:** reservation and release are idempotent per order ID. The client retries only when it *cannot know* whether a request landed (network error, 5xx, 429), never on a definite answer such as "insufficient stock".

## Known limits

Written down so they are choices you can defend rather than surprises:

| Limit | Why it is acceptable here | What removing it would take |
|---|---|---|
| **One replica of each service.** | Product's state is one file (one writer). Order's read model is in memory and only sees commands handled by that same process, so a second replica would answer queries from a view that never saw the first one's orders. | Product: a real database. Order: a change feed from the event store to every replica's read model. |
| **The reconciler polls**, rather than being triggered. | Simple, and safe because every step it repeats is idempotent. Worst case is a delay of a few seconds. | A transactional outbox, which is more moving parts for a latency win nobody needs yet. |
| **Product rewrites its whole snapshot on every change.** | Fine for a catalogue of thousands of rows; every mutation is atomic and crash-safe. | A database, once the catalogue outgrows a few MB. |
| **No authentication, authorisation, TLS or rate limiting.** Anyone who can reach a port can ship orders or change stock. | It is a teaching project and runs on localhost. | An API gateway or service mesh for TLS and auth; the admin endpoints (`POST /products`, `PUT .../stock`, `.../ship`) would be the first to need protecting. |
| **Startup replays the whole event log** to rebuild the read model. | Measured on generated logs, in this sandbox: 10,000 orders (20,000 events) start in about 0.17 s; 100,000 orders (200,000 events, 51 MB) in about 2.0 s. Linear, so a million orders would be around 20 s. | Snapshots in the read model, or a persisted projection. |
| **Spec check compares routes, not schemas.** | See [seed.md](seed.md#5-specifications-in-an-open-standard). | An OpenAPI validator dependency. |
