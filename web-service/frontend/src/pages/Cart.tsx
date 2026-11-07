import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useCart, useCheckout, useClearCart, useProducts, useRemoveItem, useSetItem } from "../api/hooks";
import type { CartLine, Product } from "../api/types";
import { Button, EmptyState, ErrorNote, Icon, Loading, Notice, PageTitle } from "../components/ui";
import { activeLines, estimate, MAX_QUANTITY } from "../lib/cart";
import { declineMessage, rejectMessage, type CheckoutOutcome } from "../lib/checkout";
import { useCustomerId } from "../lib/identity";
import { formatMoney } from "../lib/money";

// There is no card form: this demo never touches card numbers. The payment
// service accepts opaque tokens, and each one makes it behave differently, so
// the picker is also how you try the failure paths from the browser.
const METHODS = [
  { token: "tok_visa", label: "Visa ending 4242", hint: "Approved." },
  { token: "tok_declined", label: "Card that is declined", hint: "Declined. The cart is kept so you can pay another way." },
  { token: "tok_insufficient_funds", label: "Card with no funds", hint: "Declined for insufficient funds." },
  { token: "tok_expired_card", label: "Expired card", hint: "Declined as expired." },
  { token: "tok_flaky", label: "Card network that stumbles", hint: "Fails once, then works. The order still ends up paid, once." },
  { token: "tok_slow", label: "Slow card network", hint: "Approved after a few seconds. Try double-clicking Pay." },
  { token: "tok_down", label: "Card network that is down", hint: "Never answers. The order waits and then lets go of the stock." },
] as const;

function Line({ line, product, busy, onSet, onRemove }: { line: CartLine; product?: Product; busy: boolean; onSet: (q: number) => void; onRemove: () => void }) {
  const unitless = !product;
  return (
    <li className="flex flex-wrap items-center gap-x-4 gap-y-2 py-4">
      <div className="min-w-0 flex-1">
        <p className="font-semibold">{line.name || product?.name || line.product_id}</p>
        <p className="text-sm text-muted">{unitless ? "Price confirmed at checkout" : `${formatMoney(product.price_minor, product.currency)} each`}</p>
      </div>
      <div className="flex items-center" role="group" aria-label={`${line.name} quantity`}>
        <Button variant="secondary" className="!rounded-r-none !px-2.5" disabled={busy} onClick={() => onSet(line.quantity - 1)} aria-label="Remove one">
          <Icon name="minus" className="h-4 w-4" />
        </Button>
        <span className="flex h-[38px] w-10 items-center justify-center border-y border-ink/30 bg-surface text-sm font-semibold">{line.quantity}</span>
        <Button variant="secondary" className="!rounded-l-none !px-2.5" disabled={busy || line.quantity >= MAX_QUANTITY} onClick={() => onSet(line.quantity + 1)} aria-label="Add one more">
          <Icon name="plus" className="h-4 w-4" />
        </Button>
      </div>
      <span className="w-28 text-right font-display text-lg font-bold">{product ? formatMoney(product.price_minor * line.quantity, product.currency) : ""}</span>
      <Button variant="ghost" className="!px-2" onClick={onRemove} disabled={busy} aria-label={`Remove ${line.name} from the cart`}>
        <Icon name="trash" className="h-4 w-4" />
      </Button>
    </li>
  );
}

function Outcome({ outcome, onRetry, retrying }: { outcome: CheckoutOutcome; onRetry: () => void; retrying: boolean }) {
  switch (outcome.kind) {
    case "declined":
      return (
        <Notice tone="bad" title={declineMessage(outcome.code)}>
          You were not charged and your cart is exactly as it was. Choose another payment method and pay again.
        </Notice>
      );
    case "rejected":
      return (
        <Notice tone="bad" title="The order was not placed">
          {outcome.detail || rejectMessage(outcome.reason)} You were not charged.
        </Notice>
      );
    case "empty":
      return <Notice tone="warn" title="Your cart is empty">Add something from the shop first.</Notice>;
    case "invalid":
      return <Notice tone="bad" title="That was not accepted">{outcome.detail}</Notice>;
    case "unknown":
      return (
        <Notice tone="warn" title="We could not confirm the order">
          <p>The store did not answer, so we do not know yet whether your order went through. Nothing is lost, and pressing Pay again cannot order twice.</p>
          <Button variant="secondary" className="mt-3" onClick={onRetry} busy={retrying}>
            Check and pay again
          </Button>
        </Notice>
      );
    case "failed":
      return <Notice tone="bad" title="Something went wrong">{outcome.detail}</Notice>;
    default:
      return null;
  }
}

export function CartPage() {
  const customer = useCustomerId();
  const navigate = useNavigate();
  const cart = useCart(customer);
  const products = useProducts();
  const setItem = useSetItem(customer);
  const removeItem = useRemoveItem(customer);
  const clear = useClearCart(customer);
  const checkout = useCheckout(customer);
  const [token, setToken] = useState<string>(METHODS[0].token);
  const [outcome, setOutcome] = useState<CheckoutOutcome | null>(null);

  if (cart.isPending) return <Loading label="Loading your cart" />;
  if (cart.isError) return <ErrorNote error={cart.error} onRetry={() => cart.refetch()} />;

  const lines = activeLines(cart.data);
  const byId = new Map((products.data ?? []).map((p) => [p.id, p]));
  const est = estimate(lines, products.data ?? []);

  function pay() {
    setOutcome(null);
    checkout.mutate(token, {
      onSuccess: (o) => {
        if (o.kind === "placed" || o.kind === "pending") {
          navigate(`/orders/${o.order.id}`, { state: { arrived: o.kind === "pending" ? "pending" : o.replay ? "replay" : "placed" } });
        } else setOutcome(o);
      },
    });
  }

  if (lines.length === 0) {
    const ordered = cart.data.order_id;
    return (
      <>
        <PageTitle title="Cart" />
        <EmptyState title="Your cart is empty">
          {ordered ? (
            <>
              Your last cart became <Link to={`/orders/${ordered}`} className="font-semibold text-brand underline">order {ordered}</Link>.
            </>
          ) : (
            <Link to="/" className="font-semibold text-brand underline">Browse the shop</Link>
          )}
        </EmptyState>
      </>
    );
  }

  const mutating = setItem.isPending || removeItem.isPending || clear.isPending;
  const method = METHODS.find((m) => m.token === token);

  return (
    <>
      <PageTitle title="Cart">
        <Button variant="ghost" onClick={() => clear.mutate()} disabled={mutating || checkout.isPending}>
          Empty the cart
        </Button>
      </PageTitle>
      <div className="grid gap-8 lg:grid-cols-[1fr_22rem]">
        <section aria-label="Items">
          <ul className="panel divide-y divide-dotted divide-ink/30 px-5">
            {lines.map((l) => (
              <Line
                key={l.product_id}
                line={l}
                product={byId.get(l.product_id)}
                busy={mutating || checkout.isPending}
                onSet={(q) => (q <= 0 ? removeItem.mutate(l.product_id) : setItem.mutate({ productId: l.product_id, quantity: q }))}
                onRemove={() => removeItem.mutate(l.product_id)}
              />
            ))}
          </ul>
          {(setItem.error || removeItem.error) && (
            <div className="mt-4">
              <ErrorNote error={setItem.error ?? removeItem.error} />
            </div>
          )}
          <p className="mt-3 text-sm text-muted">Stock is reserved when you pay, not when you add to the cart, so something can sell out while you decide.</p>
        </section>

        <aside className="space-y-4" aria-label="Payment">
          <div className="panel p-5">
            <div className="leader">
              <span className="text-sm text-muted">{est.complete ? "Estimated total" : "Estimated total (some prices unknown)"}</span>
              <span className="font-display text-3xl font-bold tracking-tight">{formatMoney(est.minor)}</span>
            </div>
            <p className="mt-1 text-xs text-muted">The final price is the one quoted when the items are reserved.</p>

            <fieldset className="mt-5">
              <legend className="mb-2 text-sm font-semibold">Pay with</legend>
              <div className="space-y-1.5">
                {METHODS.map((m) => (
                  <label key={m.token} className={`flex cursor-pointer items-center gap-3 rounded-md border px-3 py-2 text-sm ${token === m.token ? "border-ink bg-sunken" : "border-ink/20 hover:border-ink/50"}`}>
                    <input type="radio" name="method" value={m.token} checked={token === m.token} onChange={() => setToken(m.token)} className="accent-[rgb(var(--brand))]" />
                    {m.label}
                  </label>
                ))}
              </div>
              <p className="mt-2 text-xs text-muted">{method?.hint} These are test payment methods; no card number is ever entered.</p>
            </fieldset>

            <Button className="mt-5 w-full !py-3 text-base" onClick={pay} busy={checkout.isPending} aria-live="polite">
              {checkout.isPending ? "Paying" : `Pay ${est.complete ? formatMoney(est.minor) : ""}`.trim()}
            </Button>
          </div>
          {outcome && <Outcome outcome={outcome} onRetry={pay} retrying={checkout.isPending} />}
          {checkout.isError && <ErrorNote error={checkout.error} />}
        </aside>
      </div>
    </>
  );
}
