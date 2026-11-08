# Front end: how it is built, and where to take it

For whoever works on this next. The app is finished in the sense that every backend feature is reachable and every failure the backend can produce has a screen. It is deliberately plain in structure so that the visual side can change a lot without touching behaviour.

## Run it

```
npm ci
npm run dev        # http://localhost:5173, proxies /api to http://localhost:3000 (override: GATEWAY=...)
npm test           # unit and component tests (Vitest, Testing Library)
npm run typecheck && npm run lint
npm run build      # writes ../internal/ui/dist, which the Go gateway embeds
npm run gen:api    # regenerate src/api/generated/*.d.ts from ../../docs/openapi
```

Start the back end first with `docker compose up --build` from the repository root.

## Layout

| Path | What lives there |
|---|---|
| `src/api/` | `client.ts` (fetch, problem+json to `ApiError`), `hooks.ts` (every React Query hook), `types.ts` (hand-written shapes the screens use), `generated/` (types from the OpenAPI files; do not edit) |
| `src/lib/` | Pure logic with tests: `checkout.ts` (what each status means to a shopper), `orderFlow.ts`, `cart.ts`, `money.ts`, `identity.ts`, `adminKey.ts` |
| `src/components/` | `ui.tsx` (Button, Badge, Notice and the other primitives), `Layout.tsx`, `ProductCard`, `ProductArt`, `OrderProgress`, `EventTimeline`, `StatusBadge` |
| `src/pages/` | One file per screen; `admin/` for the console |
| `src/index.css`, `tailwind.config.ts` | The design tokens and the few component classes (`.panel`, `.input`, `.receipt`, `.leader`) |

## Design system

The look is a price tag and a till receipt: concrete-grey canvas, paper panels, bottle-green ink, and one tag-magenta accent used only for prices and the primary action. Headlines and numbers are Bricolage Grotesque, body is Instrument Sans, figures are tabular. Panels are flat (no shadows, no gradients). Order pages are serrated receipts with dotted leaders; the admin event log is a ledger.

- Colours are CSS variables as `R G B` triplets in `index.css`, mapped to Tailwind tokens (`canvas`, `surface`, `sunken`, `ink`, `muted`, `line`, `brand`, `ok`, `warn`, `bad`, `info`). Change the palette there; do not hard-code colours in components. Dark mode follows the system setting and is defined in the same place.
- Status is never colour alone: every badge carries a glyph and a word.
- Products have no images in the backend, so `ProductArt` draws a deterministic SVG per product id. Replacing it with real photos means adding an `image_url` to the Product API first.

## Rules that keep it correct (please keep them)

1. **Checkout can end five ways, and they are different.** `declined` and `rejected` are definite (nothing charged, cart kept). `unknown` means no answer: say so, never say "failed", and offer a retry (safe, the key is derived from the cart). `pending` and `placed` go to the order page. `lib/checkout.ts` and its tests are the single place this is decided.
2. **Never do arithmetic on floats.** Prices are integer paise; `formatMoney` is the only formatter.
3. **The cart has no prices.** The total shown on the cart page is an estimate from the catalogue and is labelled so; the receipt shows what was actually quoted.
4. **The browser holds no secret and no identity.** Who you are comes from `GET /api/me` (a signed `HttpOnly` cookie the page cannot read); the customer id is never typed, stored or sent by the page. Do not put an id, token or key in localStorage, a URL or a log. A `401` from any call signs the page out (`main.tsx`); changing requests carry `X-Requested-With` (`api/client.ts`).
5. **The gateway is an allow-list.** A new screen that needs a new route needs that route added to `web-service/internal/httpapi/server.go` and to the service's OpenAPI file; a test fails otherwise.
6. **A checked-out cart is shown as empty** (`activeLines`), because the backend keeps it frozen so a repeated checkout can be answered.

## Obvious next steps

Product photos and a product detail page; order filtering and search for admins; toasts for admin actions; a proper empty/illustrated state per page; Playwright tests against `docker compose`.
