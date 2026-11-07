import { useState } from "react";
import { Link, useLocation, useParams } from "react-router-dom";
import { useCancelOrder, useOrder } from "../api/hooks";
import { ApiError } from "../api/client";
import type { Order } from "../api/types";
import { Button, EmptyState, ErrorNote, Loading, Notice } from "../components/ui";
import { OrderProgress } from "../components/OrderProgress";
import { StatusBadge } from "../components/StatusBadge";
import { canCancel, isSettling, whatIsHappening } from "../lib/orderFlow";
import { formatMoney } from "../lib/money";

const ARRIVED = {
  placed: { tone: "ok", title: "Order placed", body: "Thank you. This page updates by itself." },
  replay: { tone: "info", title: "That order was already placed", body: "Pressing Pay again did not order twice; this is the order you already have." },
  pending: { tone: "warn", title: "Order received, payment still being confirmed", body: "The items are held for you. This page updates by itself." },
} as const;

/** The order as a printed receipt: it is what the backend knows, set out the way a till would. */
export function Receipt({ order }: { order: Order }) {
  const reserved = order.items.every((i) => i.unit_price_minor !== undefined);
  return (
    <div className="receipt mx-auto w-full max-w-md px-7 text-sm">
      <p className="text-center font-display text-2xl font-extrabold tracking-tighter">gocart</p>
      <p className="mt-1 text-center font-mono text-xs text-muted">{order.id}</p>
      <p className="text-center text-xs text-muted">
        {new Date(order.placed_at).toLocaleString("en-IN", { dateStyle: "medium", timeStyle: "short" })} for {order.customer_id}
      </p>

      <ul className="mt-6 space-y-2 border-y border-dashed border-ink/40 py-4">
        {order.items.map((i) => (
          <li key={i.product_id} className="leader">
            <span>
              {i.quantity} × {i.name ?? i.product_id}
            </span>
            <span className="font-medium">{i.unit_price_minor !== undefined ? formatMoney(i.unit_price_minor * i.quantity, order.currency) : "pending"}</span>
          </li>
        ))}
      </ul>

      <div className="leader mt-4">
        <span className="font-display text-lg font-bold">Total</span>
        <span className="font-display text-3xl font-extrabold tracking-tight text-brand">
          {order.total_minor !== undefined ? formatMoney(order.total_minor, order.currency) : "—"}
        </span>
      </div>
      {!reserved && order.total_minor === undefined && <p className="mt-1 text-xs text-muted">Prices are quoted once the items are reserved.</p>}

      <dl className="mt-6 space-y-1 border-t border-dashed border-ink/40 pt-4 text-xs text-muted">
        <div className="flex justify-between gap-4">
          <dt>Status</dt>
          <dd className="font-semibold text-ink">{order.status.replace("_", " ")}</dd>
        </div>
        {order.payment_reference && (
          <div className="flex justify-between gap-4">
            <dt>Payment reference</dt>
            <dd className="font-mono text-ink">{order.payment_reference}</dd>
          </div>
        )}
        <div className="flex justify-between gap-4">
          <dt>Order version</dt>
          <dd className="text-ink">{order.version}</dd>
        </div>
      </dl>
    </div>
  );
}

export function OrderDetailPage() {
  const { id = "" } = useParams();
  const location = useLocation();
  const order = useOrder(id);
  const cancel = useCancelOrder();
  const [confirming, setConfirming] = useState(false);

  if (order.isPending) return <Loading label="Loading the order" />;
  if (order.isError) {
    if (order.error instanceof ApiError && order.error.status === 404) {
      return (
        <EmptyState title="No such order">
          There is no order <span className="font-mono">{id}</span>. <Link to="/orders" className="font-semibold text-brand underline">See your orders</Link>
        </EmptyState>
      );
    }
    return <ErrorNote error={order.error} onRetry={() => order.refetch()} />;
  }

  const o = order.data;
  const arrived = (location.state as { arrived?: keyof typeof ARRIVED } | null)?.arrived;
  const banner = arrived ? ARRIVED[arrived] : undefined;

  return (
    <div className="grid gap-10 lg:grid-cols-[1fr_28rem]">
      <section className="space-y-6">
        {banner && <Notice tone={banner.tone} title={banner.title}>{banner.body}</Notice>}
        <div>
          <div className="flex flex-wrap items-center gap-3">
            <h1 className="text-4xl font-bold leading-none tracking-tight sm:text-5xl">Order</h1>
            <StatusBadge status={o.status} />
          </div>
          <p className="mt-3 max-w-prose text-lg leading-relaxed">{whatIsHappening(o)}</p>
          {isSettling(o) && <p className="mt-1 text-sm text-muted">Checking again every second or two.</p>}
        </div>

        <div className="panel p-6">
          <OrderProgress order={o} />
        </div>

        {canCancel(o.status) && (
          <div>
            {!confirming ? (
              <Button variant="danger" onClick={() => setConfirming(true)}>
                Cancel this order
              </Button>
            ) : (
              <div className="panel space-y-3 p-4">
                <p className="text-sm">
                  {o.status === "confirmed" ? "Cancelling refunds the payment and puts the items back in stock." : "Cancelling lets go of the items we are holding."}
                </p>
                <div className="flex gap-2">
                  <Button variant="danger" busy={cancel.isPending} onClick={() => cancel.mutate(o.id, { onSettled: () => setConfirming(false) })}>
                    Yes, cancel it
                  </Button>
                  <Button variant="secondary" onClick={() => setConfirming(false)}>
                    Keep the order
                  </Button>
                </div>
              </div>
            )}
            {cancel.isError && (
              <div className="mt-3">
                <ErrorNote error={cancel.error} />
              </div>
            )}
          </div>
        )}
        <Link to="/orders" className="inline-block text-sm font-semibold text-brand underline">
          All orders
        </Link>
      </section>
      <Receipt order={o} />
    </div>
  );
}
