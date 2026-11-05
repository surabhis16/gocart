# Architecture

![GoCart context](diagrams/context.png)

Three services, each in its own bounded context, each owning its own data. The Order service is the only one that talks to the others: it calls `POST /reservations` and `DELETE /reservations/{order_id}` on the Product service, and `POST /charges` and `DELETE /charges/{order_id}` on the Payment service. Product and Payment never talk to each other, and neither knows the other exists.

## What makes these microservices (Unit I)

A service earns the name by being changeable and deployable without asking anyone else. In GoCart that is concrete:

- **Separate modules.** `product-service/`, `order-service/` and `payment-service/` have their own `go.mod`, their own Dockerfile and their own tests. None imports another. Each builds and tests from its own directory, and CI runs them as separate matrix jobs.
- **No shared code.** The request-ID, problem-document, metrics and admin-key helpers are a few hundred stable lines and appear in each service. That duplication is deliberate: a shared library would mean a change to it forces every service to release together, which is exactly the coordination cost the architecture exists to avoid. Duplicating a few hundred stable lines is cheaper than that coupling.
- **No shared data.** See [Data](#data-unit-ii).
- **One coordination point, written down.** The Order service's whole understanding of the Product service lives in one file, `order-service/internal/productclient/client.go`, and of the Payment service in another, `internal/paymentclient/client.go`. The contracts they rely on are [`openapi/product.yaml`](openapi/product.yaml) and [`openapi/payment.yaml`](openapi/payment.yaml).

**Reducing coordination costs** is the argument behind all of that. Coordination between teams is the thing that does not scale: every meeting to agree a change, every release train, every "can you deploy before us". The design goal is to shrink what two teams must agree on to a small, explicit, versioned contract.

### Team topology

Using the four team types and three interaction modes from *Team Topologies* (Skelton and Pais):

| Team | Type | Owns | Interacts with |
|---|---|---|---|
| **Checkout** | stream-aligned | Order service, its event log and read model | Catalog via **X-as-a-Service** (the Product API) |
| **Catalog** | stream-aligned | Product service and its data | Checkout via the same API, which it treats as a product with consumers |
| **Payments** | stream-aligned, but in practice close to a *complicated-subsystem* team | Payment service, the provider integration and the ledger | Checkout via X-as-a-Service. Provider integrations are specialist work (money, regulation, fraud), which is the usual reason to give payments its own team |
| **Platform** | platform | pipeline, Terraform, Kubernetes manifests, the container registry | both, via **X-as-a-Service** (a paved road, not tickets) |

Two additions in practice. For the first few weeks, while the reservation contract is still being found, Checkout and Catalog would use **collaboration** mode, working side by side; once it settles it hardens into X-as-a-Service, and that is the moment to freeze `openapi/product.yaml` behind versioning. And Platform's job is to make the right thing the easy thing: `scripts/ship.sh` and the CI workflows are that, in miniature.

This project has one author, so one person wears all four hats. The point of writing the topology down is that the *code* is already split along these lines: if four people did take it over tomorrow, nobody would need to change the architecture first.

## Boundaries (Unit II)

### Domain-driven design

The three bounded contexts are not just folders. Each has its own language, and the same real-world thing means something different in each:

| Concept | In the Product context | In the Order context |
|---|---|---|
| Product | A catalogue entry with a live price and a stock level. | Only an ID and a *snapshot*: the name and price quoted when stock was reserved. |
| Customer | Does not exist. | A string ID on an order. There is no customer context yet because nothing needs one. |
| Payment method | Does not exist. | An opaque token the customer supplied. The Order service never learns what is behind it. |
| Order | Does not exist. Only an `order_id` string that keys a reservation. | The central object: everything that happened to it. |
| Charge | Does not exist. | Only the *outcome* (a payment reference, or a decline code). |
| Reservation | The central object: stock held for an order. | Never stored. The order records the *outcome* (stock reserved, with these prices). |

The Payment context is a third answer to "what is an order?": to it, an order is only a key under which at most one charge exists. It does not know what was bought.

The snapshot row is the one to dwell on. If the Order service stored a product's current price and looked it up later, a price change would silently rewrite an old order. Instead the Product service returns name and price *at the moment of reservation*, and the order keeps that copy. Two contexts hold "the price", and that duplication is correct: they are answering different questions (what does it cost now / what did this customer agree to pay).

### Event storming

Event storming starts from what *happens*, in past tense, on sticky notes, and works backwards to commands and the things that decide them. The outcome for GoCart:

| Kind | Items |
|---|---|
| **Domain events** (orange) | OrderPlaced, StockReserved, OrderRejected, PaymentCaptured, PaymentDeclined, ReservationExpired, OrderCancelled, StockReleased, PaymentRefunded, OrderShipped |
| **Commands** (blue) | Place order, Cancel order, Ship order |
| **Aggregate** (yellow) | Order (decides every command above; its stream of events is its state) |
| **Policies** (lilac): "whenever X, then Y" | whenever OrderPlaced, reserve stock · whenever StockReserved, charge the customer · whenever PaymentDeclined, release stock · whenever ReservationExpired, release stock and void the charge · whenever OrderCancelled, release stock and refund |
| **External systems** (pink) | Product service, Payment service (and behind it, the card network) |
| **Hot spots** (red): the questions the session raised | What does the customer see if Product is down? What if they click twice? What if the price changes after they browse? What if the card is declined: is the stock held forever? What if the payment provider does not answer: was the customer charged? What if the customer abandons checkout? |

The first three hot spots became the `202 Accepted` outcome and the reconciler, the `Idempotency-Key`, and the price snapshot. The payment ones, added when payments were, became the decline/unknown distinction, the idempotent charge, and the expiry with a void. The policies are the saga described below. They are the only places the Order service does something *because of* an event rather than because a user asked.

### The universal sizing formula

*Microservices: Up and Running* (Mitra and Nadareishvili) offers three principles in place of a numeric formula for "how big should a service be":

1. **Start with just a few microservices**, using bounded contexts as the starting boundaries.
2. **Keep splitting** as the system and its teams grow, guided by where coordination is hurting.
3. **Be on the right trajectory** for decreasing coordination. That matters far more than whether today's sizing is perfect.

Applied here: three services is principle 1, one per bounded context, and no smaller. The case for *not* splitting further is concrete. Stock and reservations live in the Product service together because reserving is one atomic step: check every line, deduct, and record the reservation, or do none of it. Splitting "Inventory" from "Reservations" would turn that into a distributed transaction, which is the problem the design is avoiding.

The likeliest first split, principle 2, would be **Catalog** (names, descriptions, prices: read-heavy, edited by merchandisers) away from **Inventory** (stock and reservations: write-heavy, changes every order). They change at different speeds and for different reasons, so they would eventually want different owners and release rhythms. The trigger is not "it feels big", it is "two teams keep waiting on each other to change this service". Until then, splitting is cost without benefit, and the splitting direction is easier to reverse than merging.

## Data (Unit II)

### Independent deployability and data sharing

There is no database that any two services share, and that is a design rule, not an accident. A shared database would make every schema change a cross-team negotiation, and the services would no longer be independently deployable even though they run in separate containers. **Microservices embed their data**: each service's storage is private to it.

| | Product | Order | Payment |
|---|---|---|---|
| Stores | the *current state* of the catalogue and reservations | the *history* of every order | the *current state* of each order's charge |
| How | one JSON snapshot, replaced atomically on every change (write temp file, fsync, rename) | an append-only event log, fsynced on every append | the same atomic snapshot technique, one entry per order id |
| Why that shape | a catalogue is state: nobody asks what the lamp's price was on a given Tuesday | an order is a story: what happened, in what order, matters | a ledger entry is looked up by key and answered the same way every time; the Order service already holds the story |
| Recovery | crash-safe by construction: a reader sees the old file or the new, never half | a torn final write is detected and cut off at startup; corruption anywhere else refuses to start | as Product, and a corrupt ledger refuses to start rather than quietly forgetting what was charged |

Choosing different storage for each service is the point: each picks what suits it. Cassandra, in the syllabus's Unit IV, suits the Order service's event log (append-heavy, partitioned by order), and the Order service can use it. See [operations.md](operations.md#cassandra).

### Event sourcing and CQRS

**Event sourcing.** An order is never stored as a row that gets updated. It is stored as the list of events that happened to it, and its state is whatever you get by replaying them. `GET /orders/{id}/events` shows that list for any order:

```
v1  OrderPlaced
v2  StockReserved
v3  PaymentCaptured
v4  OrderCancelled
v5  StockReleased
v6  PaymentRefunded
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

## Orders are a saga

The services cannot share a transaction, so placing an order is a sequence of steps with a crash possible between any two:

1. store `OrderPlaced`: the order exists, durably;
2. ask the Product service to reserve stock; store `StockReserved` (or `OrderRejected` if Product said no). The order is now `awaiting_payment` and its total is known;
3. ask the Payment service to charge that total; store `PaymentCaptured` (or `PaymentDeclined`). The order is now `confirmed` (or `payment_failed`).

The order of steps 2 and 3 is deliberate. Stock is reserved first because a stock shortage is cheap to undo (nothing to give back) and a payment is not; and the price is needed to know what to charge.

If anything stops in between (a timeout, an outage, a crash), the order is left in `placed` or `awaiting_payment`. Those are real, queryable states, not corrupt ones. **The reconciler** (`Service.Reconcile`) finds orders that have been waiting longer than a grace period and finishes them. Because every step it repeats is idempotent per order ID (the Product service returns the existing reservation, the Payment service returns the existing charge), finishing is always safe.

### Compensations: giving things back

Several ways for an order to end leave something owed to another service. The order records each debt as state until it has been paid, so the reconciler always knows what is still outstanding:

| Order ends as | Why | Stock owed back | Money owed back |
|---|---|---|---|
| `payment_failed` | the card was declined | yes | no (nothing was charged) |
| `expired` | nobody paid within `PAYMENT_TTL` (default 15 minutes) | yes | yes: a void, in case a charge is in flight |
| `cancelled` | the customer cancelled a confirmed order | yes | yes: a refund |
| `rejected` | Product refused (no stock) | no (nothing was held) | no |

The two debts are independent. If the Payment service is down when an order is cancelled, the stock still goes back immediately and the refund is retried until it lands (`TestCancelWithPaymentDownStillReturnsTheStock`); with both services down, both debts are owed and both are paid when they return (`TestCancelWithBothServicesDownOwesBothAndPaysBothLater`). `stock_release_pending` and `refund_pending` on the order say which is still outstanding.

This is demonstrated, not just described. `scripts/outage-demo.sh` runs two acts: the Product service goes down mid-order, then the Payment service does. Each time the customer gets `202`, the order finishes by itself when the service returns, and stock and money each move exactly once. In my run, with the reconciler polling every second, the event timestamps show `StockReserved` recorded 10.8 seconds after `OrderPlaced` (Act 1) and `PaymentCaptured` 10.9 seconds after `StockReserved` (Act 2): the 10-second grace period plus a fraction of a tick. At the default 5-second interval, expect up to about 15 seconds.

Retries are safe on every side of every call:

- **Customer to Order:** the `Idempotency-Key` header. The same key and request returns the same order (`200`), a different request under the same key (including a different `payment_token`) is `422`. The key is scoped to the customer, so two customers using the same key do not collide.
- **Order to Product:** reservation and release are idempotent per order ID. The client retries only when it *cannot know* whether a request landed (network error, 5xx, 429), never on a definite answer such as "insufficient stock".
- **Order to Payment:** a charge is identified by its order ID, so asking twice returns the recorded answer. Same retry rule.

## Payments

The Payment service exists for one reason: to be the place where "the money" is a hard problem, and to show what that does to the services around it. There is no real money. Charges go to a mock card network (`payment-service/internal/gateway`) whose behaviour is chosen by the `payment_token`, the way real providers' test cards work, so declines and outages can be produced on demand.

### Three answers, not two

A charge can come back approved, declined, or not at all, and the difference between the last two is the whole difficulty:

| Answer | Meaning | What the Order service does |
|---|---|---|
| approved (`201`/`200`) | the money was taken | records `PaymentCaptured`, order confirmed |
| declined (`402`) | the card network said no; definite | records `PaymentDeclined`, releases the stock, tells the customer (`402`) |
| no answer (`503`, timeout, connection lost) | **unknown**: the money may or may not have been taken | decides nothing; the order stays `awaiting_payment` and the charge is asked for again |

Treating "no answer" as a decline would cancel orders that were paid for. Treating it as an approval would ship goods nobody paid for. `TestLostResponseIsNotADeclineAndIsNeverChargedTwice` covers the dangerous case: the charge succeeds, the response is lost, and the retry must find the existing charge, not make a second one. The provider's side of this is `tok_slow`, which approves *after* the caller's timeout, which is how real charges succeed while the caller never hears about it.

### One charge per order

The Payment service keys everything on the order ID, and serialises work per order while the provider is being called. So 20 simultaneous requests to charge one order produce one provider call and one reference (`TestConcurrentDuplicateChargesProduceOneCharge`), and a slow charge for one order does not hold up another's. A repeat with a different amount is refused (`charge-mismatch`) rather than treated as a new charge. A decline is also recorded and replayed: a customer with a different card places a different order.

### The tombstone

Cancelling needs care at one moment: an order is abandoned while its charge request is still in flight. If the void arrived first and found nothing to void, a later-arriving charge would succeed on an order that is already over. So voiding an order with no charge records a *tombstone*, and any charge for that order afterwards is refused (`charge-voided`, `409`). If the void arrives while a charge is mid-call, it waits for that call to finish and then refunds it. Expiry uses the same mechanism, which is why `expired` owes a void even though the order never confirmed (`TestOrderExpiringDuringAChargeEndsUpRefunded`).

### What is never stored

A payment token is an opaque reference to a payment method, never a card number. The Order service refuses anything that is not shaped like `tok_` plus lowercase letters, digits and underscores, *before* writing the event, because an append-only log is the worst possible place for a card number: it can never be edited out. The Payment service applies the same rule independently. The token is kept in `OrderPlaced` so a payment that could not complete can be retried without asking the customer again; it is never returned by the API, and `GET /orders/{id}/events` strips it from the payload. (The log file itself does hold it. A real system would store a customer's saved payment-method id and let the provider own the secret.)

## Known limits

Written down so they are choices you can defend rather than surprises:

| Limit | Why it is acceptable here | What removing it would take |
|---|---|---|
| **One replica of each service.** | Product's and Payment's state is one file each (one writer), and Payment's one-charge-per-order guarantee relies on a lock inside the process. Order's read model is in memory and only sees commands handled by that same process, so a second replica would answer queries from a view that never saw the first one's orders. | Product and Payment: a real database that can enforce uniqueness across replicas. Order: a change feed from the event store to every replica's read model. |
| **The reconciler polls**, rather than being triggered. | Simple, and safe because every step it repeats is idempotent. Worst case is a delay of a few seconds. | A transactional outbox, which is more moving parts for a latency win nobody needs yet. |
| **Product rewrites its whole snapshot on every change.** | Fine for a catalogue of thousands of rows; every mutation is atomic and crash-safe. | A database, once the catalogue outgrows a few MB. |
| **Only a shared admin key; no per-user auth, TLS or rate limiting.** Shipping, stock changes, the event log and charge look-ups need `ADMIN_KEY`. Nothing identifies customers, so anyone who knows an order id can cancel it. And the Payment service's `POST /charges` and `DELETE /charges/{order_id}` are open to anyone who can reach its port. | It is a teaching project and runs on localhost. | Customer identity (tokens) and ownership checks on orders; a NetworkPolicy or mutual TLS so only the Order service can reach Payment; an API gateway for TLS and rate limiting. |
| **The payment provider is a mock, and the token is stored in the event log.** | There is no real money, and the token is an opaque test string. | A real provider integration with its own idempotency keys, webhooks for late outcomes, and saved payment-method ids instead of tokens. |
| **Unpaid stock is held for a fixed time** (`PAYMENT_TTL`, 15 minutes), checked by the polling reconciler. | Simple, and safe: expiry also voids any in-flight charge. | A per-order timer or a delayed message, and a reservation that the Product service itself expires. |
| **Startup replays the whole event log** to rebuild the read model. | Measured on generated logs, in this sandbox: 10,000 orders (20,000 events) start in about 0.17 s; 100,000 orders (200,000 events, 51 MB) in about 2.0 s. Linear, so a million orders would be around 20 s. | Snapshots in the read model, or a persisted projection. |
| **Spec check compares routes, not schemas.** | See [seed.md](seed.md#5-specifications-in-an-open-standard). | An OpenAPI validator dependency. |
