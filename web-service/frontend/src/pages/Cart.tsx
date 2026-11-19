import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import {
  useCart,
  useCheckout,
  useClearCart,
  useProducts,
  useRemoveItem,
  useSetItem,
  useSetWishlistItem,
  useWishlist,
} from "../api/hooks";
import type { CartLine, Product } from "../api/types";
import { Button, EmptyState, ErrorNote, Icon, Loading, Notice, PageTitle } from "../components/ui";
import { activeLines, estimate, MAX_QUANTITY } from "../lib/cart";
import { declineMessage, rejectMessage, type CheckoutOutcome } from "../lib/checkout";
import { useCustomerId } from "../lib/identity";
import { formatMoney } from "../lib/money";
import type { Tone } from "../lib/orderFlow";

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

function Line({
  line,
  product,
  busy,
  saving,
  canSave,
  onSet,
  onRemove,
  onSaveForLater,
}: {
  line: CartLine;
  product?: Product;
  busy: boolean;
  saving: boolean;
  canSave: boolean;
  onSet: (q: number) => void;
  onRemove: (btn: HTMLButtonElement) => void;
  onSaveForLater: () => void;
}) {
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
      <Button
        variant="ghost"
        className="text-xs text-muted hover:text-ink"
        onClick={onSaveForLater}
        disabled={busy || !canSave}
        busy={saving}
        aria-label={`Save ${line.name} for later`}
      >
        Save for later
      </Button>
      <Button
        variant="ghost"
        className="!px-2"
        onClick={(e) => onRemove(e.currentTarget)}
        disabled={busy}
        aria-label={`Remove ${line.name} from the cart`}
      >
        <Icon name="trash" className="h-4 w-4" />
      </Button>
    </li>
  );
}

function RemoveConfirmModal({
  item,
  product,
  wishlistReady,
  isProcessing,
  action,
  error,
  onRemove,
  onMoveToWishlist,
  onCancel,
}: {
  item: CartLine;
  product?: Product;
  wishlistReady: boolean;
  isProcessing: boolean;
  action: "removing" | "moving" | null;
  error: { tone: Tone; title: string; message?: string } | null;
  onRemove: () => void;
  onMoveToWishlist: () => void;
  onCancel: () => void;
}) {
  const dialogRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const focusable = dialogRef.current?.querySelectorAll<HTMLElement>("button:not([disabled])");
    focusable?.[0]?.focus();
  }, []);

  useEffect(() => {
    function onWindowKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") {
        if (!isProcessing) {
          e.preventDefault();
          onCancel();
        }
        return;
      }
      if (e.key === "Tab" && dialogRef.current) {
        const focusables = Array.from(
          dialogRef.current.querySelectorAll<HTMLElement>("button:not([disabled])"),
        );
        const first = focusables[0];
        const last = focusables[focusables.length - 1];
        if (!first || !last) return;

        if (e.shiftKey) {
          if (document.activeElement === first || !dialogRef.current.contains(document.activeElement)) {
            e.preventDefault();
            last.focus();
          }
        } else {
          if (document.activeElement === last || !dialogRef.current.contains(document.activeElement)) {
            e.preventDefault();
            first.focus();
          }
        }
      }
    }

    window.addEventListener("keydown", onWindowKeyDown);
    return () => window.removeEventListener("keydown", onWindowKeyDown);
  }, [isProcessing, onCancel]);

  const displayName = item.name || product?.name || item.product_id;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-ink/50 p-4 backdrop-blur-sm"
      onClick={(e) => {
        if (e.target === e.currentTarget && !isProcessing) {
          onCancel();
        }
      }}
      role="presentation"
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="remove-modal-title"
        aria-describedby="remove-modal-description"
        className="panel relative w-full max-w-md space-y-4 border border-ink/20 bg-surface p-6 shadow-xl"
      >
        <div className="space-y-1">
          <h3 id="remove-modal-title" className="font-display text-xl font-bold text-ink">
            Remove item from cart?
          </h3>
          <p id="remove-modal-description" className="text-sm text-muted">
            What would you like to do with this product?
          </p>
        </div>

        <div className="flex items-center justify-between rounded-md border border-ink/10 bg-sunken p-3 text-sm">
          <span className="font-semibold text-ink">{displayName}</span>
          <span className="text-muted">Quantity: {item.quantity}</span>
        </div>

        {error && (
          <Notice tone={error.tone} title={error.title}>
            {error.message}
          </Notice>
        )}

        <div className="flex flex-col gap-2 pt-2">
          <Button
            variant="danger"
            onClick={onRemove}
            disabled={isProcessing}
            busy={action === "removing"}
            className="w-full"
          >
            Remove from cart
          </Button>
          <Button
            variant="secondary"
            onClick={onMoveToWishlist}
            disabled={isProcessing || !wishlistReady}
            busy={action === "moving"}
            className="w-full"
          >
            Move to Wishlist
          </Button>
          <Button
            variant="ghost"
            onClick={onCancel}
            disabled={isProcessing}
            className="w-full"
          >
            Cancel
          </Button>
        </div>
      </div>
    </div>
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
  const wishlist = useWishlist(customer);
  const products = useProducts();
  const setItem = useSetItem(customer);
  const removeItem = useRemoveItem(customer);
  const setWishlistItem = useSetWishlistItem(customer);
  const clear = useClearCart(customer);
  const checkout = useCheckout(customer);
  const [token, setToken] = useState<string>(METHODS[0].token);
  const [outcome, setOutcome] = useState<CheckoutOutcome | null>(null);
  const [savingId, setSavingId] = useState<string | null>(null);
  const [notice, setNotice] = useState<{ tone: Tone; title: string; message?: string } | null>(null);
  const [confirmItem, setConfirmItem] = useState<CartLine | null>(null);
  const [modalAction, setModalAction] = useState<"removing" | "moving" | null>(null);
  const [modalError, setModalError] = useState<{ tone: Tone; title: string; message?: string } | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);

  const isModalProcessing = modalAction !== null;

  const handleCloseModal = () => {
    if (isModalProcessing) return;
    setConfirmItem(null);
    setModalError(null);
    setTimeout(() => {
      if (triggerRef.current && document.body.contains(triggerRef.current)) {
        triggerRef.current.focus();
      }
    }, 0);
  };

  async function handleModalRemoveFromCart() {
    if (isModalProcessing || !confirmItem) return;
    setModalError(null);
    setModalAction("removing");
    try {
      await removeItem.mutateAsync(confirmItem.product_id);
      setConfirmItem(null);
      setModalError(null);
      setTimeout(() => {
        if (triggerRef.current && document.body.contains(triggerRef.current)) {
          triggerRef.current.focus();
        }
      }, 0);
    } catch {
      setModalError({
        tone: "bad",
        title: "Could not remove item",
        message: `Failed to remove ${confirmItem.name} from your cart. Please try again.`,
      });
    } finally {
      setModalAction(null);
    }
  }

  async function handleModalMoveToWishlist() {
    if (isModalProcessing || !confirmItem) return;
    setModalError(null);

    if (!wishlist.isSuccess) {
      setModalError({
        tone: "warn",
        title: "Wishlist is not ready",
        message: "Please wait until your wishlist finishes loading.",
      });
      return;
    }

    const existingWishlistItem = (wishlist.data?.items ?? []).find(
      (it) => it.product_id === confirmItem.product_id,
    );
    const existingWishlistQty = existingWishlistItem ? existingWishlistItem.quantity : 0;
    const combined = existingWishlistQty + confirmItem.quantity;

    if (combined > MAX_QUANTITY) {
      setModalError({
        tone: "warn",
        title: "Cannot move to wishlist",
        message: `Moving ${confirmItem.name} would combine your cart quantity (${confirmItem.quantity}) with your saved wishlist quantity (${existingWishlistQty}), exceeding the maximum allowed of ${MAX_QUANTITY}. The full quantity cannot be preserved, so the item remains in your cart.`,
      });
      return;
    }

    setModalAction("moving");
    try {
      // Step 1: Destination-first -> Write to Wishlist FIRST
      await setWishlistItem.mutateAsync({
        productId: confirmItem.product_id,
        quantity: combined,
      });

      // Step 2: Only after Wishlist succeeds, remove from Cart
      try {
        await removeItem.mutateAsync(confirmItem.product_id);
        setNotice({
          tone: "ok",
          title: "Moved to wishlist",
          message: `Moved ${confirmItem.name} to your wishlist.`,
        });
        setConfirmItem(null);
        setModalError(null);
        setTimeout(() => {
          if (triggerRef.current && document.body.contains(triggerRef.current)) {
            triggerRef.current.focus();
          }
        }, 0);
      } catch {
        // Step 3: If Wishlist save succeeds but Cart removal fails, retain both copies
        // and keep modal open with clear warning so user can retry removal.
        setModalError({
          tone: "warn",
          title: "Partially moved",
          message: `Saved ${confirmItem.name} to your wishlist, but could not remove it from your cart. Both copies have been kept; please try removing from cart again.`,
        });
      }
    } catch {
      // Step 4: If Wishlist saving fails, do not remove Cart item and keep modal open
      setModalError({
        tone: "bad",
        title: "Could not move to wishlist",
        message: `Failed to save ${confirmItem.name} to your wishlist. The item remains in your cart.`,
      });
    } finally {
      setModalAction(null);
    }
  }

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

  async function handleSaveForLater(line: CartLine) {
    setNotice(null);
    if (!wishlist.isSuccess) {
      setNotice({
        tone: "warn",
        title: "Wishlist is not ready",
        message: "Please wait until your wishlist finishes loading.",
      });
      return;
    }

    const existingWishlistItem = (wishlist.data?.items ?? []).find(
      (it) => it.product_id === line.product_id,
    );
    const existingWishlistQty = existingWishlistItem ? existingWishlistItem.quantity : 0;
    const combined = existingWishlistQty + line.quantity;

    if (combined > MAX_QUANTITY) {
      setNotice({
        tone: "warn",
        title: "Cannot save for later",
        message: `Saving ${line.name} would combine your cart quantity (${line.quantity}) with your saved wishlist quantity (${existingWishlistQty}), exceeding the maximum allowed of ${MAX_QUANTITY}. The full quantity cannot be preserved, so the item remains in your cart.`,
      });
      return;
    }

    setSavingId(line.product_id);
    try {
      // Step 1: Destination-first -> Write to Wishlist FIRST
      await setWishlistItem.mutateAsync({
        productId: line.product_id,
        quantity: combined,
      });

      // Step 2: Only after Wishlist succeeds, remove from Cart
      try {
        await removeItem.mutateAsync(line.product_id);
        setNotice({
          tone: "ok",
          title: "Saved for later",
          message: `Saved ${line.name} to your wishlist.`,
        });
      } catch {
        // Step 3: If Wishlist save succeeds but Cart removal fails, retain both copies
        // and display a clear partial-success warning.
        setNotice({
          tone: "warn",
          title: "Partially saved",
          message: `Saved ${line.name} to your wishlist, but could not remove it from your cart. Both copies have been kept; you can remove it from your cart manually.`,
        });
      }
    } catch {
      // Step 4: If Wishlist saving fails, do not remove the Cart item.
      setNotice({
        tone: "bad",
        title: "Could not save for later",
        message: `Failed to save ${line.name} to your wishlist. The item remains in your cart.`,
      });
    } finally {
      setSavingId(null);
    }
  }

  if (lines.length === 0) {
    const ordered = cart.data.order_id;
    return (
      <>
        <PageTitle title="Cart" />
        {notice && (
          <div className="mb-4">
            <Notice tone={notice.tone} title={notice.title}>
              {notice.message}
            </Notice>
          </div>
        )}
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

  const mutating = setItem.isPending || removeItem.isPending || clear.isPending || savingId !== null || isModalProcessing;
  const method = METHODS.find((m) => m.token === token);

  return (
    <>
      <PageTitle title="Cart">
        <Button variant="ghost" onClick={() => clear.mutate()} disabled={mutating || checkout.isPending}>
          Empty the cart
        </Button>
      </PageTitle>

      {notice && (
        <div className="mb-4">
          <Notice tone={notice.tone} title={notice.title}>
            {notice.message}
          </Notice>
        </div>
      )}

      <div className="grid gap-8 lg:grid-cols-[1fr_22rem]">
        <section aria-label="Items">
          <ul className="panel divide-y divide-dotted divide-ink/30 px-5">
            {lines.map((l) => (
              <Line
                key={l.product_id}
                line={l}
                product={byId.get(l.product_id)}
                busy={mutating || checkout.isPending}
                saving={savingId === l.product_id}
                canSave={wishlist.isSuccess}
                onSet={(q) => (q <= 0 ? removeItem.mutate(l.product_id) : setItem.mutate({ productId: l.product_id, quantity: q }))}
                onRemove={(btn) => {
                  triggerRef.current = btn;
                  setModalError(null);
                  setConfirmItem(l);
                }}
                onSaveForLater={() => handleSaveForLater(l)}
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

      {confirmItem && (
        <RemoveConfirmModal
          item={confirmItem}
          product={byId.get(confirmItem.product_id)}
          wishlistReady={wishlist.isSuccess}
          isProcessing={isModalProcessing}
          action={modalAction}
          error={modalError}
          onRemove={handleModalRemoveFromCart}
          onMoveToWishlist={handleModalMoveToWishlist}
          onCancel={handleCloseModal}
        />
      )}
    </>
  );
}
