import type { Order, OrderStatus } from "../api/types";

/** The happy path, in order. Failure states are shown beside it, not on it. */
export const HAPPY_PATH = ["placed", "paid", "shipped", "delivered"] as const;
export type Step = (typeof HAPPY_PATH)[number];

const RANK: Partial<Record<OrderStatus, number>> = {
  placed: 0,
  awaiting_payment: 0,
  confirmed: 1,
  shipped: 2,
  delivered: 3,
};

/** How far along the happy path an order has got, or -1 if it left the path. */
export function progress(status: OrderStatus): number {
  return RANK[status] ?? -1;
}

/** Statuses that will still change on their own; the page keeps polling while it sees one. */
export function isSettling(o: Pick<Order, "status" | "stock_release_pending" | "refund_pending">): boolean {
  return (
    o.status === "placed" ||
    o.status === "awaiting_payment" ||
    o.stock_release_pending === true ||
    o.refund_pending === true
  );
}

export function canCancel(status: OrderStatus): boolean {
  return status === "placed" || status === "awaiting_payment" || status === "confirmed";
}

export const STATUS_LABEL: Record<OrderStatus, string> = {
  placed: "Placed",
  awaiting_payment: "Awaiting payment",
  confirmed: "Confirmed",
  rejected: "Rejected",
  payment_failed: "Payment failed",
  expired: "Expired",
  cancelled: "Cancelled",
  shipped: "Shipped",
  delivered: "Delivered",
};

export type Tone = "ok" | "warn" | "bad" | "info" | "neutral";

export const STATUS_TONE: Record<OrderStatus, Tone> = {
  placed: "info",
  awaiting_payment: "warn",
  confirmed: "ok",
  rejected: "bad",
  payment_failed: "bad",
  expired: "neutral",
  cancelled: "neutral",
  shipped: "info",
  delivered: "ok",
};

export const ALL_STATUSES = Object.keys(STATUS_LABEL) as OrderStatus[];

/** One plain sentence on what is going on, for the person waiting. */
export function whatIsHappening(o: Order): string {
  switch (o.status) {
    case "placed":
      return "We have your order and are reserving the items.";
    case "awaiting_payment":
      return "The items are held for you. We are waiting for the payment to go through; this settles by itself.";
    case "confirmed":
      return "Payment taken. The order is being prepared.";
    case "shipped":
      return "On its way.";
    case "delivered":
      return "Delivered.";
    case "rejected":
      return o.reject_detail || "The order could not be placed. You were not charged.";
    case "payment_failed":
      return "The payment did not go through. You were not charged, and the items are back in stock.";
    case "expired":
      return "The payment did not arrive in time, so the order was released. You were not charged.";
    case "cancelled":
      return o.refund_pending
        ? "Cancelled. The refund is on its way."
        : "Cancelled. Anything charged has been refunded.";
  }
}
