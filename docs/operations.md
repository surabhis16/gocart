# Running, deploying and changing GoCart

Everything here is free to run. Nothing needs a cloud account, a licence key or a credit card.

| Tool | Used for | Cost |
|---|---|---|
| Go 1.22 or newer | building and testing | free, open source |
| Docker (Engine, or Docker Desktop) | containers, Compose | free; Docker Desktop is free for personal and educational use |
| minikube + kubectl | local Kubernetes | free, open source |
| Terraform (or OpenTofu) | infrastructure as code | free to use; Terraform is source-available (BSL), and **OpenTofu** is the open-source drop-in, where you type `tofu` for `terraform` |
| GitHub Actions + ghcr.io | CI and the container registry | free for public repositories; private repositories get a free monthly allowance |
| LocalStack, AWS | **not used** | see [Infrastructure as code](#infrastructure-as-code-unit-iii) |

## Run it

### Fastest: Docker Compose

```
docker compose up --build
```

Product is on <http://localhost:8080>, Order on <http://localhost:8081>, Payment on <http://localhost:8082>, Cart on <http://localhost:8083>, and the storefront on <http://localhost:3000> (the admin console is at `/admin`; the key is `dev-admin-key` unless you set `ADMIN_KEY`). The Product service seeds a small demo catalogue on first start, including two awkward items on purpose: a lamp with only 3 in stock, and a desk mat that is sold out.

```
curl -s localhost:8080/products | python3 -m json.tool

curl -s -X POST localhost:8081/orders \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: demo-1' \
  -d '{"customer_id":"cust_asha","payment_token":"tok_visa","items":[{"product_id":"prd_keyboard","quantity":2}]}'
```

There is no real money. `payment_token` picks how the mock card network behaves: `tok_visa` (or any `tok_...`) is approved, `tok_declined`, `tok_insufficient_funds` and `tok_expired_card` are declined, `tok_flaky` fails once then works, `tok_down` never answers, `tok_slow` answers after a delay. The full table is in [`openapi/payment.yaml`](openapi/payment.yaml). Try a declined one and look at the status code (`402`), then at the lamp's stock (it went back).

Then look at what was actually stored, which is the event-sourcing demo. The event log and the charge are admin-only, so they need the key (see [Admin key](#admin-key-and-metrics)):

```
curl -s -H 'Authorization: Bearer dev-admin-key' localhost:8081/orders/<id-from-above>/events | python3 -m json.tool
```

You will see `OrderPlaced`, `StockReserved`, `PaymentCaptured`. The payment token is deliberately not shown (see [architecture.md](architecture.md#payments)).

Three scripts exercise it end to end (use a fresh stack for `smoke.sh`: `docker compose down -v`):

- `./scripts/smoke.sh`: 82 checks across all five services: confirmed, retried, rejected, declined, flaky card, cancelled and refunded, cart and checkout (a declined card keeps the cart, a repeated checkout is one order), shipped then delivered, admin endpoints refused without the key, the gateway serving the app and refusing internal routes, metrics present.
- `./scripts/oversell-demo.sh`: 20 customers each click "buy" twice at the same instant, for 3 lamps. It checks that exactly 3 orders were confirmed, stock ended at 0, exactly 3 charges were taken and no double-click was charged twice. It sets the stock itself, so it can be re-run.
- `./scripts/outage-demo.sh`: two acts. The Product service goes down mid-order, then the Payment service does; each time the order is accepted (`202`), and finishes by itself when the service returns, with stock reserved once and money taken once.

### Without Docker

```
export ADMIN_KEY=dev-admin-key
(cd product-service && go run .)                       # :8080
(cd payment-service && go run .)                       # :8082
(cd order-service   && go run .)                       # :8081
(cd cart-service    && go run .)                       # :8083
(cd web-service/frontend && npm ci && npm run build)   # once; needs Node 22
(cd web-service     && go run .)                       # :3000
```

For front-end work, `make web-dev` serves the app with hot reload on <http://localhost:5173>, proxying `/api` to a running gateway. Without the build step the gateway still runs and says the front end is not built.

Data goes to `./data` in each directory. Configuration is by environment variable; the full list is at the top of each `main.go`.

### Tests

```
make test
```

Format check, `go vet` and the race detector for all five Go modules: 227 tests (276 counting subtests). The front end has its own (`cd web-service/frontend && npm test`, 20 tests, run in CI with type-check, lint and build). The Cassandra tests only compile with `-tags cassandra` and need a live Cassandra; see below.

## Developer workspace (Unit IV)

**Coding standards** are enforced by the machine instead of by review comments: CI fails on anything `gofmt` would change, on `go vet` findings, and on any data race (`go test -race`). Each service is a standard Go layout: `main.go` for wiring, `internal/` for everything else, so nothing outside a service can import its internals.

**A containerised environment locally** is `docker-compose.yml`. Each of the four Dockerfiles is a two-stage build: compile in `golang:1.24-alpine`, then copy the single static binary into `scratch`, an empty image with no shell, no package manager and nothing to patch. Because there is no `curl` in such an image, each binary carries its own `-healthcheck` flag, which is what the `HEALTHCHECK` instruction and the compose health condition call. Containers run as a non-root user, and data lives in named volumes.

### Cassandra

The Order service stores its events in a file by default. To use Cassandra instead:

```
docker compose -f docker-compose.yml -f docker-compose.cassandra.yml up --build
```

This builds the Order service with `-tags cassandra`, starts a single Cassandra node (heap capped for laptops, about 1.5 GB of memory in all), and points the Order service at it. The first start is slow because Cassandra takes a minute or more to boot; the Order service waits for it and creates its own keyspace and table.

How the Cassandra store works is in `order-service/internal/eventstore/cassandra.go`: one partition per order, one row per event, and `INSERT ... IF NOT EXISTS` (a lightweight transaction) so that of any number of writers racing to append the same version of the same order, exactly one wins.

> **Read this about verification.** The Cassandra store is the one part of this project I could not compile against the real driver or run: my build environment could not download Go modules or pull images. I type-checked the code against a stub of the driver API, and the file store and Cassandra store are held to *one shared test suite* (`contract_test.go`). The CI job `cassandra-event-store` compiles it against the real driver and runs that suite against a live Cassandra. **Until that job is green, treat the Cassandra store as untested.** To run it yourself:
>
> ```
> docker run -d --name cass -p 9042:9042 cassandra:5.0      # wait about a minute
> cd order-service
> go get github.com/apache/cassandra-gocql-driver/v2@v2.0.0 && go mod tidy
> go vet -tags cassandra ./...
> CASSANDRA_TEST_HOSTS=127.0.0.1 go test -tags cassandra -v -run Cassandra ./internal/eventstore
> ```
>
> If `go vet -tags cassandra` reports a signature mismatch against the driver, that is the thing to fix, and it will point at the exact line.

### Kubernetes

Two ways to run on a local cluster, both under `infra/k8s/` and built with Kustomize (included in `kubectl`):

| Overlay | For | Images come from |
|---|---|---|
| `overlays/local` | Docker Desktop's Kubernetes, or minikube with `minikube image load` | images already on the node |
| `overlays/staging` | minikube, the full shipping flow below | a registry inside the cluster |
| `overlays/staging-cassandra` | staging, plus Cassandra and the Order service switched over | same |

```
docker compose build
kubectl apply -k infra/k8s/overlays/local
kubectl -n gocart-local get pods
```

The manifests make a few choices on purpose; each is commented in the YAML:

- **One replica each, with a `Recreate` rollout.** State is a file with one writer; see the [limits table](architecture.md#known-limits). The exception is `staging-cassandra`, which runs **two Order replicas** with a `RollingUpdate` (no volume, nothing on disk to protect). They stay consistent through the change feed; tune it with `SYNC_INTERVAL` (default 2s) and `RESYNC_INTERVAL` (default 5m). To see it: `kubectl -n gocart-staging get pods -l app.kubernetes.io/name=order-service`, place orders, and read them back repeatedly: both replicas answer, and `GET /orders/{id}` is always right.
- **The Order service's readiness probe checks neither the Product nor the Payment service.** If it did, an outage of either would take Order out of rotation, and the whole design is that Order keeps accepting orders during one.
- **The admin key and the sign-in secrets come from Secrets**, never from a literal in a Deployment. The `local` and `staging` overlays generate them from demo values; see [Admin key](#admin-key-and-metrics) and [Sign-in, secrets and the edge](#sign-in-secrets-and-the-edge).
- **Only the gateway is reachable, and only through the ingress.** NetworkPolicies deny all inbound traffic, then allow the ingress controller to reach the gateway and Dex, and each service to be reached only by its real callers.
- **Hardened pods:** non-root, read-only root filesystem, all capabilities dropped, default seccomp profile, resource limits.
- **Cassandra** is a one-node StatefulSet with a headless Service and a volume claim template.

`python3 scripts/check-k8s.py` validates all of it without a cluster: every object against the Kubernetes 1.31 API schemas (unknown fields are errors), then selector/label matches, probe ports, Service targets, overlay references and image tags against `VERSION`.

## Infrastructure as code (Unit III)

### DevOps principles, in this repository

| Principle | Where |
|---|---|
| Everything is in version control, including infrastructure | `infra/` |
| Every change is built and tested automatically | `.github/workflows/ci.yml` |
| The pipeline is the only path to a release | tag `v*` triggers `release.yml` |
| Small, frequent, reversible changes | one `VERSION`, one-command bump (`scripts/bump-version.sh`), idempotent operations |
| Environments are reproducible | `docker compose up`, `kubectl apply -k` |
| Know when it is broken | structured JSON logs, a request ID across all five services, a Prometheus `/metrics` endpoint on each, health endpoints |

### Terraform

`infra/terraform/` describes what GoCart would need from AWS: a container registry (ECR) and a log group (CloudWatch) for each of the five services, and one IAM policy that grants a CI job the minimum needed to push images and nothing else. ECR repositories are `IMMUTABLE` (a tag names one image forever, so "what is running in staging?" always has an answer), scan on push, and expire all but the newest ten images.

There is deliberately **no VPC, load balancer or cluster**. Those are the expensive parts of AWS, and this project runs on a laptop. The practice, which is what Unit III teaches, is infrastructure described in files, reviewed, planned, and applied by a pipeline.

**You can run `plan` for free with no AWS account.** The `offline` variable (default `true`) gives the AWS provider throw-away credentials and tells it not to look anything up. Nothing in the configuration reads from AWS, so the plan is complete:

```
make tf-plan
```

or, step by step:

```
cd infra/terraform
terraform init -backend=false
terraform validate
terraform plan -var offline=true
```

The plan shows 16 resources to add: 5 repositories, 5 lifecycle policies, 5 log groups and 1 IAM policy (the policy document is a data source, computed locally, so it is not counted). That is the real output of `terraform plan -var offline=true`.

> **What is verified.** `terraform validate` passes and `terraform plan -var offline=true` produces the 16 additions above, run by the author; CI runs `fmt -check`, `init`, `validate` and `plan` on every push and is green. Applying to real AWS needs an account and `-var offline=false`; that has not been done and nothing here claims the configuration applies cleanly. Free-tier LocalStack was not used, because I could not confirm that its free edition supports ECR.

### The pipeline

`ci.yml` runs four jobs on every push and pull request:

| Job | What it checks |
|---|---|
| `test` | per service: `gofmt`, `go vet`, `go test -race` (including the OpenAPI drift test) |
| `cassandra-event-store` | compiles the Cassandra store and runs the shared contract suite against a live Cassandra |
| `infrastructure` | `terraform fmt/validate/plan`, the Kubernetes check, both compose files |
| `end-to-end` | builds the four images, starts the stack, runs `smoke.sh`, the oversell demo, then the outage demo |

## Releasing (Unit V)

### The staging environment, shipping and deploying

```
./scripts/ship.sh               # or: ./scripts/ship.sh cassandra
```

This is the whole path from source to a running staging environment:

1. confirms the manifests agree on the version in `VERSION`;
2. enables minikube's `registry` addon, a registry running inside the cluster;
3. opens it on `localhost:5000` on your machine;
4. builds the four images, tags them `localhost:5000/gocart/<service>:<version>`, and pushes them;
5. applies the `staging` overlay into the `gocart-staging` namespace, which makes the cluster pull the *same name* from its own registry;
6. waits for each rollout, then prints how to reach it.

Then `./scripts/smoke.sh` against the forwarded ports, or the outage demo using `kubectl scale` as the way to take Product and Payment away (the exact commands are printed).

For a release outside your machine, pushing a tag such as `v0.2.0` makes `release.yml` build all four images and publish them to GitHub's free container registry, `ghcr.io`.

`scripts/bump-version.sh 0.2.0` changes the version everywhere it is written (the `VERSION` file, the manifests, the compose defaults) and re-runs the consistency check. Left to hand edits, those drift apart and staging quietly runs a different image than the one you just shipped. I tested it by bumping up, bumping back, and confirming the tree is byte-identical to where it started.

> **Verified.** `ship.sh` has been run end to end for the Cassandra staging flow by the author, and passes `shellcheck` and `bash -n`. The most likely thing to need adjusting on another machine is the registry port-forward, which behaves slightly differently across operating systems and minikube drivers; the comments in the script explain the mechanism so you can debug it.

### Clean up

```
./scripts/cleanup.sh
```

Deletes the `gocart-staging` and `gocart-local` namespaces (which removes Deployments, Services, volumes and Cassandra with them), stops the registry port-forward, removes the local images, and runs `docker compose down -v`. "Clean up" should leave nothing running.

## Managing change (Unit V)

Changes to a microservices system are mostly changes to *contracts* between services, since each service's insides can change freely. Here are the kinds of change, and what GoCart does for each.

### Additive change: no coordination needed

Suppose the Product service adds an optional field (say `sku`) to its responses. The Order service must keep working with no change and no release. That works because the two sides are deliberately asymmetric:

- **The Order service reads Product's responses tolerantly.** It ignores fields it does not know. `TestReserveSuccessIgnoresFieldsItDoesNotKnow` puts `some_future_field` in a response and checks nothing breaks.
- **The Product service reads requests strictly.** It rejects unknown fields (`TestStrictBodyParsing`). Silently ignoring a misspelt field is how `"quantity"` becomes `0`, so for input it is better to fail loudly.

This is Postel's law applied in the right direction: strict about what you accept from untrusted callers, tolerant of what you receive from a service you depend on.

### Breaking change: run both versions side by side

Suppose the Product service must rename `price_minor`. GoCart has no `/v1` prefix yet; the first breaking change is when one appears. The safe sequence is *expand, migrate, contract*:

1. **Expand.** Add `POST /v2/reservations` next to the existing `/reservations`. Both are live. Nothing breaks.
2. **Migrate.** Release the Order service to call `/v2`. Because both exist, the two services can release in either order, which is the point.
3. **Contract.** When no traffic reaches the old path (the access log shows this: every request has path and status), delete it.

At no step do two teams need to deploy together.

### A real breaking change, and how it was handled

Version 0.2 made `payment_token` a **required** field on `POST /orders`. That is a breaking change to a customer-facing contract: a client written against 0.1 now gets `422`. It was acceptable here for three reasons, and they are the questions to ask before any breaking change: the version is 0.x (no stability promise yet), an order without a payment method cannot complete, so there was no sensible default, and there is exactly one known client (the scripts, updated in the same change). With real clients the safe route is the one above: accept orders without a token for a while, answer them with an `awaiting_payment` order the client completes later, and only then require it.

### Change to stored data: events are forever

The Order service's events are history and are never edited. The rules the code already enforces:

- Adding an *optional* field to an event is safe: old events simply lack it.
- Changing the *meaning* of an event is not. Add a new event type instead.
- **A real case: `OrderConfirmed`.** Version 0.1 confirmed an order the moment stock was reserved and wrote `OrderConfirmed`. Version 0.2 splits that into `StockReserved` (stock held, amount worked out) and `PaymentCaptured` (money taken). The old events are facts in existing logs and cannot be rewritten, so 0.2 keeps *reading* `OrderConfirmed` (an order confirmed that way is marked as having had no payment step, so cancelling it releases stock but does not try to refund money that was never taken) and never *writes* it. `TestVersion01LogsStillReplay` and `TestVersion01OrdersSurviveTheUpgrade` hold this in place. The rule: a new version must read every event any earlier version wrote.
- Reading an event type the code does not know **refuses to start** (`TestApplyRefusesDamagedHistory`). That is the right behaviour if an old binary meets a newer log: silently skipping an event would change what an order is. The cost is that you cannot roll the Order service back past a release that introduced a new event type without migrating the log. Know that before you ship one. Version 0.2 is exactly such a release: it can read a 0.1 log, but a 0.1 binary cannot read a log that 0.2 has added `StockReserved` to.

### Change under failure: retries must be safe

Every operation that crosses a network is idempotent, which turns "the network lost the reply" from a data-corruption problem into a non-event: `Idempotency-Key` for customers, per-order-ID reservation, release, charge and void between services. [architecture.md](architecture.md#orders-are-a-saga) has the detail.

## Admin key and metrics

### Admin key

Some operations are not for customers: shipping an order, reading an order's raw event log, creating or restocking products, looking up a charge. Those require `Authorization: Bearer <ADMIN_KEY>`. Without the header, or with the wrong key, the answer is `401`. A service started with **no** `ADMIN_KEY` refuses every admin request rather than leaving them open, and says so in its log. The key is compared in constant time.

This is a shared secret, which is the simplest thing that is honestly better than nothing. It is not per-user authentication, and it does not authenticate the Order service to the Payment service: the charge and void endpoints are open to anyone who can reach the Payment service's port. In a cluster, a NetworkPolicy would restrict that port to the Order service's pods; a service mesh would make it mutual TLS. Both are out of scope, and listed in the [known limits](architecture.md#known-limits).

Compose and the Kubernetes overlays use the demo value `dev-admin-key`. **Do not use it anywhere real.** To use your own:

```
ADMIN_KEY=$(openssl rand -hex 24) docker compose up --build
kubectl -n gocart-staging create secret generic gocart-admin --from-literal=admin-key="$(openssl rand -hex 24)" --dry-run=client -o yaml | kubectl apply -f -
```

and remove the `secretGenerator` from the overlay so it does not overwrite yours.

### Sign-in, secrets and the edge

Customers sign in through Dex (an OpenID Connect provider) and the gateway vouches for them to the Order and Cart services; how it works is in [architecture.md](architecture.md#who-is-asking-sign-in-and-ownership).

**Compose, first run (no setup).** `docker compose up --build`, open <http://localhost:3000>, press *Sign in*. Dex is on :5556 and, with nothing configured, offers three demo accounts (password `gocart-demo`): `asha@gocart.test`, `ravi@gocart.test` and `admin@gocart.test` (the administrator). Sign in as Asha, add to a cart, then as Ravi in a private window: his cart and orders are his own, and her order's address answers `404`.

**Compose, with Google or GitHub.** Dex offers each sign-in method whose settings you give it, in a `.env` file (copy `.env.example`; git ignores `.env`). Creating a customer is automatic: a first sign-in is the sign-up, and the customer id comes from the provider's account id.

1. *Google.* In [Google Cloud Console](https://console.cloud.google.com/apis/credentials) create a project if you have none, configure the OAuth consent screen (External; add yourself as a test user), then Credentials, *Create credentials*, *OAuth client ID*, type *Web application*. Under *Authorized redirect URIs* add `http://localhost:5556/dex/callback`: **Dex's** callback, not the shop's. Copy the client ID and secret into `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET`.
2. *GitHub.* In GitHub, Settings, Developer settings, OAuth Apps, *New OAuth App*. Homepage `http://localhost:3000`, callback `http://localhost:5556/dex/callback`. Put the ID and a generated secret in `GITHUB_CLIENT_ID` and `GITHUB_CLIENT_SECRET`.
3. Set `ADMIN_EMAILS` to your own address (the one the provider reports as verified), and `DEX_DEMO_USERS=false` so the published demo password no longer works.
4. `docker compose up -d --force-recreate dex web-service`. The sign-in page now lists the methods you configured. With a single method, pressing *Sign in* goes straight to it.

If a method you set up is missing, `docker compose logs dex` says which variable is empty. The login pages are styled by `infra/k8s/base/dex/theme` (the same theme files compose and Kubernetes both use).

**Kubernetes with HTTPS.** Traefik (the ingress controller; the older community ingress-nginx was retired in March 2026) and cert-manager are installed once per cluster, then the `local-https` overlay adds the front door. Helm is used only for Traefik's own install; nothing of GoCart needs it.

```
helm repo add traefik https://traefik.github.io/charts && helm repo update
helm install traefik traefik/traefik -n traefik --create-namespace   # note the version it installs, then pin it with --version
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
kubectl apply -f infra/k8s/cert-manager/clusterissuers.yaml          # once cert-manager's pods are Ready
kubectl apply -k infra/k8s/overlays/local-https
kubectl -n traefik port-forward svc/traefik 8443:443
```

Then open <https://gocart.localtest.me:8443> (that name always resolves to 127.0.0.1; the browser warns once about the self-signed certificate). The shop must be opened at this address: the gateway's `PUBLIC_URL`, Dex's issuer and redirect URI, and the route's host have to agree, and `check-k8s.py` fails if they do not. The plain `local` and `staging` overlays have no front door: they are for `ship.sh` and `smoke.sh`, which call the services directly. Port-forwarding to the `web-service` Service serves the pages there, but sign-in cannot complete, because the browser is sent to the public address.

Traefik rate-limits each client address (a steady 20 requests a second with bursts to 60, and 2 a second with bursts to 10 on `/auth/`; see `infra/k8s/edge/edge.yaml`) and caps request bodies at 64 KiB. To try it: `for i in $(seq 60); do curl -sk -o /dev/null -w '%{http_code} ' https://gocart.localtest.me:8443/auth/login; done` and watch `429` appear.

**Kubernetes does not read `.env`.** That file only feeds docker compose. In the cluster the same settings live in the `gocart-auth` Secret (the secrets and the four Google and GitHub values) and the `gocart-public` ConfigMap (`dex-demo-users`, `admin-emails`, `signin-hint`), and the `local` and `staging` overlays create both with demo values. Do not patch them by hand: the next `kubectl apply -k` puts the demo values back. Instead keep your own values in a private overlay that is applied on top:

```
cp -r infra/k8s/overlays/local-https-private.example infra/k8s/overlays/local-https-private
cd infra/k8s/overlays/local-https-private
cp secrets.env.example secrets.env && cp public.env.example public.env   # fill both in
cd - && kubectl apply -k infra/k8s/overlays/local-https-private
```

`secrets.env` and `public.env` are ignored by git. The pods roll by themselves, because Kustomize renames the generated Secret and ConfigMap when their contents change. For a staging cluster, make the same overlay on top of `staging`.

**Google and GitHub on Kubernetes (Kubernetes does not read `.env`).** The `.env` file only feeds Docker Compose. In the cluster the same settings live in two objects the overlay creates with demo values: the `gocart-auth` Secret (the signing secrets, plus `google-client-id`, `google-client-secret`, `github-client-id`, `github-client-secret`) and the `gocart-public` ConfigMap (`dex-demo-users`, `admin-emails`, `signin-hint`). Editing `.env` therefore changes nothing there, and Dex shows no Google or GitHub buttons until the Secret holds the keys.

Register the same two apps as for compose, but with your public address: the callback is `https://<your host>/dex/callback` (locally `https://gocart.localtest.me:8443/dex/callback`). Then keep your real values in a **private overlay** that sits on top of the shared one, instead of patching live objects (a later `kubectl apply -k` would put the demo values back):

```
infra/k8s/overlays/local-https-private/
  kustomization.yaml
  secrets.env      # google-client-id=...  google-client-secret=...  github-client-id=...  github-client-secret=...
  public.env       # dex-demo-users=false  and  admin-emails=you@example.com
```

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../local-https
secretGenerator:
  - name: gocart-auth
    behavior: merge
    envs:
      - secrets.env
configMapGenerator:
  - name: gocart-public
    behavior: merge
    envs:
      - public.env
```

`kubectl apply -k infra/k8s/overlays/local-https-private` then regenerates both objects, updates the hashed ConfigMap name in the Deployments, and the pods that read them restart by themselves. `.gitignore` excludes `*.env`, so the secrets never reach git; do not commit the two `.env` files, and give each teammate their own. If you must patch live objects instead, remember that Kustomize adds a hash to the ConfigMap's name (`kubectl get cm` shows the real one), that pods read environment variables only when they start (restart the Deployments), and that the next apply will overwrite your patch. In `staging`, which `ship.sh` deploys, do the same with a private overlay on top of `staging`.

The `local` and `staging` overlays are demos: they switch the throwaway accounts on (`dex-demo-users=true`) and name `admin@gocart.test` the administrator, which a private overlay replaces.

**Secrets.** Three values sign and verify: `IDENTITY_SECRET` (shared by the gateway, Order and Cart; at least 32 characters, or the service refuses to start), `SESSION_SECRET` (the gateway only) and `OIDC_CLIENT_SECRET` (the gateway and Dex). The demo values are literals in compose and the overlays. For anything real:

```
kubectl -n gocart-staging create secret generic gocart-auth \
  --from-literal=identity-secret="$(openssl rand -hex 32)" \
  --from-literal=session-secret="$(openssl rand -hex 32)" \
  --from-literal=oidc-client-secret="$(openssl rand -hex 24)" --dry-run=client -o yaml | kubectl apply -f -
```

and remove the `gocart-auth` generator from the overlay. Changing `SESSION_SECRET` signs everyone out; changing `IDENTITY_SECRET` needs the gateway, Order and Cart restarted together. **Real use** means turning the demo accounts off and signing in through Google or GitHub (above), or pointing the gateway at another OIDC provider directly (set `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `ADMIN_EMAILS`), a real hostname with `letsencrypt.example.yaml` in place of the self-signed issuer, and keeping the secrets in an external store rather than in the overlay.

**Calling the services by hand** (for debugging) now needs an identity: `scripts/mint.py identity "$IDENTITY_SECRET" cust_x` prints the header value to send as `X-Gocart-Identity`. `smoke.sh` does this for you; set `IDENTITY_SECRET=` (empty) to run it against services started without sign-in.

### Metrics

Each service serves Prometheus-format metrics at `/metrics`, written by hand (the format is small and stable, and it keeps the services dependency-free): request counts and latency histograms labelled by *route pattern* (`GET /orders/{id}`, never the raw path, so a million order ids are one series, not a million), plus numbers from each service's own domain: orders by status and the reconciler's backlog (Order), products, products out of stock and units in stock (Product), charges by status (Payment).

```
curl -s localhost:8081/metrics | grep gocart_
```

The two to watch are in the Order service: `gocart_orders_pending_work` with `kind` `placed` or `awaiting_payment` (something downstream is unavailable), and with `kind` `stock_release` or `refund` (a compensation is not landing). All four are the reconciler's backlog, and all should drain to zero.

## Measuring progress (Unit V)

The syllabus asks how to tell whether adopting microservices is working. The honest answer is to measure *flow and coordination*, not the number of services. The four standard delivery metrics (from the DORA research), and where each comes from in this repository:

| Metric | What it tells you | Source here |
|---|---|---|
| **Deployment frequency** | How often you can safely ship | count of `v*` tags and `release.yml` runs |
| **Lead time for changes** | Commit to running in staging | CI run time plus `ship.sh` duration |
| **Change failure rate** | How often a release breaks something | red `end-to-end` runs on `main` / total |
| **Time to restore** | How fast service comes back | measured in the outage demo, from event timestamps: 10.8 s from `OrderPlaced` to `StockReserved` after Product returned, and 10.9 s from `StockReserved` to `PaymentCaptured` after Payment returned (1 s reconcile interval, 10 s grace) |

And two that matter specifically for *this* architecture, because reducing coordination is its whole premise:

- **Cross-team changes per release:** how many releases needed more than one team to change something. It should trend towards zero, and a service whose changes always drag another service along is the sign of a boundary in the wrong place.
- **Splits and merges:** per the sizing formula, a healthy system keeps splitting where coordination hurts. Track *why* each boundary moved.

A team that ships twice a day to four services, and almost never has to ask the other team before doing so, has made progress. One that has twenty services and a weekly joint release has not.

## Complexity and simplification

Microservices do not remove complexity, they move it: from inside a codebase to between services, where it is harder to see. GoCart's wager is that three services is the least that demonstrates the benefits (the third, Payment, is where the cost became real), and the cost is visible in the code: the saga and its compensations, the reconciler, idempotency, tombstones and retries are exactly the complexity that a single process gets for free from a transaction. Anything added to a boundary should earn its place against that bill.
