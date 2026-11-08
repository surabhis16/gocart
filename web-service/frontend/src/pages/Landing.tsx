import { Link, useSearchParams } from "react-router-dom";
import { useProducts } from "../api/hooks";
import { Notice } from "../components/ui";
import { StatusBadge } from "../components/StatusBadge";
import { signInUrl, useMe } from "../lib/identity";
import { formatMoney } from "../lib/money";
import type { OrderStatus } from "../api/types";

const SIGNIN_PROBLEMS: Record<string, { title: string; body: string }> = {
  unavailable: { title: "Sign-in is not available right now", body: "The sign-in service did not answer. You can still browse the shop; try signing in again in a minute." },
  failed: { title: "Sign-in did not complete", body: "Something went wrong between here and the sign-in service. Try again." },
  expired: { title: "That sign-in took too long", body: "Start again and finish within ten minutes." },
  denied: { title: "Sign-in was cancelled", body: "You did not sign in, so nothing was changed." },
  unverified: { title: "That email address is not verified", body: "The sign-in service says this address has not been confirmed, so it cannot be used here." },
};

const primary = "inline-flex items-center justify-center rounded-md bg-brand px-6 py-3 text-base font-semibold text-brand-ink transition hover:opacity-90";
const secondary = "inline-flex items-center justify-center rounded-md border border-ink/40 px-6 py-3 text-base font-semibold transition hover:border-ink hover:bg-sunken";

/** The real price list, set out as a till receipt: whatever the Product service says right now. */
function PriceList() {
  const products = useProducts();
  const shown = (products.data ?? []).slice(0, 5);
  return (
    <div className="receipt mx-auto w-full max-w-md rotate-[1.2deg] px-7 text-sm">
      <p className="text-center font-display text-2xl font-extrabold tracking-tighter">gocart</p>
      <p className="mt-1 text-center text-xs text-muted">Price list, read live from the Product service</p>
      <ul className="mt-6 space-y-2.5 border-y border-dashed border-ink/40 py-4" aria-live="polite">
        {products.isPending && [0, 1, 2, 3].map((i) => <li key={i} className="h-5 animate-pulse rounded-sm bg-sunken" />)}
        {products.isError && <li className="text-muted">The catalogue is not reachable right now.</li>}
        {shown.map((p) => (
          <li key={p.id} className="leader">
            <span className="font-medium">{p.name}</span>
            <span className="font-display text-base font-bold">{formatMoney(p.price_minor, p.currency)}</span>
          </li>
        ))}
      </ul>
      <p className="mt-4 text-center text-xs text-muted">
        {products.data ? `${products.data.length} products, ${products.data.reduce((n, p) => n + p.stock, 0)} units on the shelf` : " "}
      </p>
    </div>
  );
}

interface Row {
  event: string;
  who: string;
  what: string;
}

/** An order's history, as the Order service really records it. */
function Ledger({ title, rows, ends }: { title: string; rows: Row[]; ends: OrderStatus }) {
  return (
    <section className="panel p-6" aria-label={title}>
      <h3 className="font-display text-2xl font-bold tracking-tight">{title}</h3>
      <ol className="relative mt-5 ml-3 border-l-2 border-ink/80">
        {rows.map((r, i) => (
          <li key={r.event} className="mb-5 ml-6">
            <span className="absolute -left-[7px] mt-1.5 h-3 w-3 border-2 border-ink bg-surface" />
            <p className="flex flex-wrap items-baseline gap-x-2">
              <span className="font-mono text-xs text-muted">v{i + 1}</span>
              <span className="font-semibold">{r.event}</span>
              <span className="text-xs text-muted">{r.who}</span>
            </p>
            <p className="mt-0.5 text-sm text-muted">{r.what}</p>
          </li>
        ))}
        <li className="ml-6 flex items-center gap-2 pt-1 text-sm">
          <span className="text-muted">The order is</span>
          <StatusBadge status={ends} />
        </li>
      </ol>
    </section>
  );
}

const SERVICES: [string, string][] = [
  ["Product", "the catalogue, prices and stock"],
  ["Cart", "what you intend to buy"],
  ["Order", "each order's history, and its state"],
  ["Payment", "charges, and giving them back"],
  ["Web", "sign-in, and this app"],
];

export function Landing() {
  const me = useMe();
  const [params] = useSearchParams();
  const problem = SIGNIN_PROBLEMS[params.get("signin") ?? ""];
  const signedIn = me.data?.signed_in === true;
  const hint = me.data && !me.data.signed_in ? me.data.hint : "";

  return (
    <div className="space-y-24 pb-8">
      {problem && (
        <Notice tone="warn" title={problem.title}>
          {problem.body}
        </Notice>
      )}

      <section className="grid items-center gap-14 lg:grid-cols-[1.15fr_1fr]">
        <div>
          <h1 className="text-5xl font-extrabold leading-[0.95] tracking-tighter sm:text-6xl lg:text-7xl">
            Every price on a tag. Every order on a receipt.
          </h1>
          <p className="mt-6 max-w-[34rem] text-lg leading-relaxed text-muted">
            GoCart is a small store built from five cooperating services. Buy something, then open the receipt and the ledger of facts it was worked out from.
          </p>
          <div className="mt-8 flex flex-wrap gap-3">
            {signedIn ? (
              <Link to="/shop" className={primary}>Open the shop</Link>
            ) : (
              <a href={signInUrl("/shop")} className={primary}>Sign in to shop</a>
            )}
            <Link to={signedIn ? "/orders" : "/shop"} className={secondary}>{signedIn ? "See your orders" : "Browse the catalogue"}</Link>
          </div>
          {hint && <p className="mt-5 max-w-[34rem] text-sm text-muted">{hint}</p>}
        </div>
        <PriceList />
      </section>

      <section aria-labelledby="pay-heading">
        <h2 id="pay-heading" className="max-w-2xl text-4xl font-bold leading-tight tracking-tight sm:text-5xl">
          What happens when you press Pay
        </h2>
        <p className="mt-4 max-w-[38rem] text-lg leading-relaxed text-muted">
          An order is never edited. Each step is a fact added to its history, and the order you see is worked out from those facts. That is why a failed payment can always be explained, and why a repeated click cannot charge twice.
        </p>
        <div className="mt-10 grid gap-6 lg:grid-cols-2">
          <Ledger
            title="The card is approved"
            ends="confirmed"
            rows={[
              { event: "OrderPlaced", who: "Order service", what: "The order is written down before anything else, so it cannot be lost." },
              { event: "StockReserved", who: "Product service", what: "The items are held for you, at a price quoted then." },
              { event: "PaymentCaptured", who: "Payment service", what: "The card is charged once, even if the request is repeated." },
            ]}
          />
          <Ledger
            title="The card is declined"
            ends="payment_failed"
            rows={[
              { event: "OrderPlaced", who: "Order service", what: "Recorded exactly as before." },
              { event: "StockReserved", who: "Product service", what: "The items are held while the card is tried." },
              { event: "PaymentDeclined", who: "Payment service", what: "The card network said no. Nothing is charged." },
              { event: "StockReleased", who: "Product service", what: "The items go straight back on the shelf for someone else." },
            ]}
          />
        </div>
      </section>

      <section aria-labelledby="services-heading" className="grid gap-10 lg:grid-cols-[1fr_1.1fr]">
        <div>
          <h2 id="services-heading" className="text-4xl font-bold leading-tight tracking-tight sm:text-5xl">
            Five services, one front door
          </h2>
          <p className="mt-4 max-w-[30rem] text-lg leading-relaxed text-muted">
            Each service owns its own data and can be stopped and restarted on its own. The browser only ever talks to the last one.
          </p>
        </div>
        <ul className="space-y-3 self-center">
          {SERVICES.map(([name, job]) => (
            <li key={name} className="leader text-lg">
              <span className="font-display font-bold">{name}</span>
              <span className="text-muted">{job}</span>
            </li>
          ))}
        </ul>
      </section>
    </div>
  );
}
