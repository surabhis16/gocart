import { ApiError, problemKind, type RawResponse } from "../api/client";
import type { Order, Problem } from "../api/types";

/**
 * What a checkout attempt means for the person at the screen. The Cart service
 * passes the Order service's answer through, so every outcome here corresponds
 * to a status the backend really gives (see docs/openapi/cart.yaml).
 */
export type CheckoutOutcome =
  | { kind: "placed"; order: Order; replay: boolean }
  | { kind: "pending"; order: Order }
  | { kind: "declined"; code: string; orderId?: string }
  | { kind: "rejected"; reason: string; detail: string }
  | { kind: "empty" }
  | { kind: "invalid"; detail: string }
  /** No answer: it is not known whether an order was made. Repeating the same request is safe. */
  | { kind: "unknown" }
  | { kind: "failed"; detail: string };

export function interpretCheckout(res: RawResponse): CheckoutOutcome {
  const { status } = res;
  if (status === 201 || status === 200) {
    return { kind: "placed", order: res.body as Order, replay: status === 200 };
  }
  if (status === 202) return { kind: "pending", order: res.body as Order };

  const p = (res.body ?? {}) as Problem;
  const kind = problemKind(p);
  const detail = typeof p.detail === "string" ? p.detail : "";

  if (status === 402 || kind === "payment-declined") {
    return {
      kind: "declined",
      code: typeof p.decline_code === "string" ? p.decline_code : "card_declined",
      orderId: typeof p.order_id === "string" ? p.order_id : undefined,
    };
  }
  if (kind === "cart-empty") return { kind: "empty" };
  if (kind === "order-rejected") {
    return { kind: "rejected", reason: typeof p.reason === "string" ? p.reason : "refused", detail };
  }
  if (status === 422 || status === 400) return { kind: "invalid", detail: detail || "The request was not accepted." };
  if (status === 0 || status >= 500 || status === 429 || status === 408) return { kind: "unknown" };
  return { kind: "failed", detail: detail || `Unexpected response (${status}).` };
}

/** Wraps a thrown network failure as "no answer", the same as a 503. */
export function interpretFailure(e: unknown): CheckoutOutcome {
  if (e instanceof ApiError && e.status === 0) return { kind: "unknown" };
  throw e;
}

const DECLINE_TEXT: Record<string, string> = {
  card_declined: "The card was declined.",
  insufficient_funds: "The card has insufficient funds.",
  expired_card: "The card has expired.",
};
export const declineMessage = (code: string) => DECLINE_TEXT[code] ?? "The payment was declined.";

const REJECT_TEXT: Record<string, string> = {
  insufficient_stock: "Not enough stock for one of the items. Lower the quantity or remove it.",
  unknown_product: "One of the products no longer exists.",
  invalid_items: "Something in the cart is not valid.",
  refused: "The order could not be placed.",
};
export const rejectMessage = (reason: string) => REJECT_TEXT[reason] ?? "The order could not be placed.";
