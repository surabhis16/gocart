# Changes in 0.5.0 (from 0.4.0)

Adds real sign-in, a landing page, and an HTTPS edge. Before this, a customer id was a label kept in the browser and anyone could read anyone's cart or order by guessing an id.

## New
- **Sign-in with OpenID Connect**, handled by the web gateway (authorization code flow with PKCE; the ID token is checked for signature, issuer, audience, expiry and nonce). The session is a signed `HttpOnly` cookie, so no session store and any gateway replica can serve any request. Administrators are people whose verified email is on `ADMIN_EMAILS`; the admin key now lives only in the gateway.
- **Ownership is enforced in the services.** The gateway adds a short-lived signed identity (`X-Gocart-Identity`) to calls to the Order and Cart services; they refuse a customer acting in someone else's name (`403`) and show another customer's order as `404`. `REQUIRE_IDENTITY=true` (compose, Kubernetes) makes the customer routes fail closed. The OpenAPI files document the header, `401` and `403`.
- **Landing page** in the existing theme: a live price-list receipt, the real event history of an approved and a declined order, and the five services. The storefront browses publicly; cart, orders and the admin console need a sign-in. The admin-key box and the browser-stored customer id are gone.
- **Dex** (compose and Kubernetes) as the identity provider, configured from the environment: "Log in with Google" and "Log in with GitHub" switch on when their client IDs are set (`.env.example`, `docs/operations.md`), so there are no accounts or passwords in the repository's default config. Three throwaway demo accounts (password published) stay available behind `DEX_DEMO_USERS=true`, which compose and the demo overlays turn on. The Dex login pages are restyled in the GoCart theme (`infra/k8s/base/dex/theme`), and the browser-tab icon is now the price tag instead of the old indigo cart.
- **Kubernetes:** NetworkPolicies (default deny, then only real callers), HTTPS through Traefik with a cert-manager certificate and per-address rate limits (stricter on `/auth/`), as an opt-in `local-https` overlay (ingress-nginx was retired in March 2026), a self-signed issuer for local use and a Let's Encrypt example. `check-k8s.py` now also checks policies against who calls whom, that the routes, middlewares and certificate line up, and that the public address agrees across the gateway, Dex and the routes.
- `scripts/mint.py` makes the signed values for tests; `smoke.sh` has 99 checks (17 new: anonymous refusals, ownership between customers through the gateway and directly on the services, admin access, CSRF, spoofed headers).

## Compatibility
- **Breaking for direct callers.** With `REQUIRE_IDENTITY=true` the Order and Cart customer routes need the identity header, so a script calling them directly needs `scripts/mint.py`. Without the flag, services run as before for local use. The gateway refuses to start without `PUBLIC_URL`, an OIDC issuer and the secrets.
- New environment: `IDENTITY_SECRET` (32+ characters), `SESSION_SECRET`, `OIDC_*`, `PUBLIC_URL`, `ADMIN_EMAILS`. Compose and the overlays carry demo values.
- On Kubernetes the shop is opened through Traefik (overlay `local-https`) at `https://gocart.localtest.me:8443`; a plain port-forward to the gateway no longer completes sign-in.

## Numbers
279 Go tests (331 with subtests, all with `-race`), 25 front-end tests.

## Verified
In the sandbox: all Go and front-end tests, type-check, lint, build, `check-k8s.py`, `smoke.sh` 99/99 on real processes, and the full sign-in flow in a real browser against a stand-in OIDC provider. **Not run anywhere yet:** Dex itself (its login templates with our theme and its config template were checked separately, see the README), Google and GitHub sign-in, Traefik, cert-manager, the NetworkPolicies, the Docker build of this version, CI.

## Known limits (added to the architecture table)
A session cannot be revoked before its 8 hours end; Dex's accounts and the demo secrets are for local use only.

# Earlier: 0.4.0 (from 0.3.0)

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

## Verified
In the sandbox: all Go tests, the front-end tests, type-check, lint and production build; the five real processes driven in a real browser; `check-k8s.py`. On the author's machine and in CI: the five images build and start under Compose, `smoke.sh` passes 82 of 82, `terraform validate` passes and the plan adds 16 resources, the Cassandra job and the full CI run are green, and the Cassandra staging flow runs on Kubernetes. The README table lists each part and who ran it.

## Fixed after first build
`frontend/scripts/clean-dist.mjs` failed inside the Docker build because the embed folder did not exist there yet. It now creates the folder first (found and fixed by the author).
