import type { Order } from "../api/types";
import { HAPPY_PATH, progress } from "../lib/orderFlow";
import { Icon } from "./ui";

const LABEL = { placed: "Placed", paid: "Paid", shipped: "Shipped", delivered: "Delivered" } as const;

/**
 * The customer's view of an order: four steps and how far along it is. An order
 * that left the happy path (rejected, cancelled, ...) shows no progress bar;
 * the page explains what happened in words instead.
 */
export function OrderProgress({ order }: { order: Order }) {
  const at = progress(order.status);
  if (at < 0) return null;
  // "placed" and "awaiting payment" are both still at the first step.
  const waiting = order.status === "placed" || order.status === "awaiting_payment";
  return (
    <ol className="flex items-center" aria-label="Order progress">
      {HAPPY_PATH.map((step, i) => {
        const done = i < at || (i === at && !waiting) || order.status === "delivered";
        const current = i === at && waiting;
        return (
          <li key={step} className="flex flex-1 items-center last:flex-none" aria-current={current ? "step" : undefined}>
            <div className="flex flex-col items-center gap-1.5">
              <span
                className={`flex h-8 w-8 items-center justify-center rounded-sm border-2 text-sm font-bold ${
                  done
                    ? "border-ink bg-ink text-surface"
                    : current
                      ? "border-brand text-brand"
                      : "border-ink/25 text-muted"
                }`}
              >
                {done ? <Icon name="check" className="h-4 w-4" /> : i + 1}
              </span>
              <span className={`text-xs ${done || current ? "font-semibold text-ink" : "text-muted"}`}>{LABEL[step]}</span>
            </div>
            {i < HAPPY_PATH.length - 1 && <span className={`mx-2 mb-5 flex-1 border-t-2 ${i < at ? "border-solid border-ink" : "border-dashed border-ink/25"}`} />}
          </li>
        );
      })}
    </ol>
  );
}
