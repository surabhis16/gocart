import type { OrderEvent } from "../api/types";
import { clock } from "../lib/time";
import { Badge } from "./ui";
import type { Tone } from "../lib/orderFlow";

// What each fact means in words. These are the event types in
// docs/openapi/order.yaml; an unknown one still renders, under its own name.
const MEANING: Record<string, { text: string; tone: Tone }> = {
  OrderPlaced: { text: "Order accepted and written to the log", tone: "info" },
  StockReserved: { text: "Stock reserved at the Product service", tone: "ok" },
  OrderRejected: { text: "Order rejected, nothing was reserved or charged", tone: "bad" },
  PaymentCaptured: { text: "Payment taken at the Payment service", tone: "ok" },
  PaymentDeclined: { text: "Payment declined, the order cannot go ahead", tone: "bad" },
  ReservationExpired: { text: "Payment did not arrive in time, stock released", tone: "neutral" },
  OrderCancelled: { text: "Order cancelled", tone: "neutral" },
  StockReleased: { text: "Reserved stock returned to the shelf", tone: "neutral" },
  PaymentRefunded: { text: "Charge voided or refunded", tone: "neutral" },
  OrderShipped: { text: "Handed to the carrier", tone: "info" },
  OrderDelivered: { text: "Reached the customer", tone: "ok" },
  OrderConfirmed: { text: "Confirmed (legacy event from version 0.1)", tone: "ok" },
};

/** The order's event log as a vertical timeline: the actual facts its state is derived from. */
export function EventTimeline({ events }: { events: OrderEvent[] }) {
  return (
    <ol className="relative ml-3 border-l-2 border-ink/80">
      {events.map((e) => {
        const m = MEANING[e.type] ?? { text: "", tone: "neutral" as Tone };
        const hasData = Object.keys(e.data ?? {}).length > 0;
        return (
          <li key={e.version} className="mb-5 ml-6 last:mb-0">
            <span className="absolute -left-[7px] mt-1.5 h-3 w-3 border-2 border-ink bg-surface" />
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-mono text-xs text-muted">v{e.version}</span>
              <Badge tone={m.tone}>{e.type}</Badge>
              <time className="text-xs text-muted" dateTime={e.at} title={e.at}>
                {clock(e.at)}
              </time>
            </div>
            {m.text && <p className="mt-1 text-sm text-muted">{m.text}</p>}
            {hasData && (
              <details className="mt-1.5">
                <summary className="cursor-pointer text-xs text-muted hover:text-ink">Event data</summary>
                <pre className="mt-1 overflow-x-auto rounded-md border border-ink/20 bg-sunken p-3 font-mono text-xs">{JSON.stringify(e.data, null, 2)}</pre>
              </details>
            )}
          </li>
        );
      })}
    </ol>
  );
}
