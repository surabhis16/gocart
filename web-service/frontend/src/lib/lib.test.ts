import { describe, expect, it } from "vitest";
import { activeLines, estimate, itemCount, maxFor, MAX_QUANTITY } from "./cart";
import { formatMoney } from "./money";
import { canCancel, isSettling, progress } from "./orderFlow";
import { declineMessage, interpretCheckout, interpretFailure, rejectMessage } from "./checkout";
import { ApiError, problemKind } from "../api/client";
import type { Cart, Product } from "../api/types";

const res = (status: number, body: unknown = {}) => ({ status, body });
const problem = (kind: string, extra: object = {}) => ({ type: `urn:gocart:problem:${kind}`, ...extra });

describe("formatMoney", () => {
  it("formats paise as rupees with Indian grouping", () => {
    expect(formatMoney(79900)).toBe("₹799.00");
    expect(formatMoney(12345600)).toBe("₹1,23,456.00");
  });
});

describe("interpretCheckout", () => {
  const order = { id: "ord_1" };
  it("maps success statuses", () => {
    expect(interpretCheckout(res(201, order))).toEqual({ kind: "placed", order, replay: false });
    expect(interpretCheckout(res(200, order))).toEqual({ kind: "placed", order, replay: true });
    expect(interpretCheckout(res(202, order))).toEqual({ kind: "pending", order });
  });
  it("treats a decline as a definite no", () => {
    expect(interpretCheckout(res(402, problem("payment-declined", { decline_code: "expired_card" })))).toMatchObject({ kind: "declined", code: "expired_card" });
  });
  it("recognises empty cart and refused orders", () => {
    expect(interpretCheckout(res(409, problem("cart-empty")))).toEqual({ kind: "empty" });
    expect(interpretCheckout(res(409, problem("order-rejected", { reason: "insufficient_stock", detail: "x" })))).toMatchObject({ kind: "rejected", reason: "insufficient_stock" });
  });
  it("treats no answer as unknown, never as a refusal", () => {
    for (const s of [0, 500, 503, 504, 429, 408]) expect(interpretCheckout(res(s)).kind).toBe("unknown");
  });
  it("reports validation problems and surprises", () => {
    expect(interpretCheckout(res(422, { detail: "bad token" }))).toEqual({ kind: "invalid", detail: "bad token" });
    expect(interpretCheckout(res(418)).kind).toBe("failed");
  });
  it("turns a network failure into unknown and rethrows anything else", () => {
    expect(interpretFailure(new ApiError("offline", 0))).toEqual({ kind: "unknown" });
    expect(() => interpretFailure(new Error("bug"))).toThrow("bug");
  });
  it("has wording for every code, with a fallback", () => {
    expect(declineMessage("insufficient_funds")).toMatch(/insufficient/);
    expect(declineMessage("nope")).toMatch(/declined/);
    expect(rejectMessage("insufficient_stock")).toMatch(/stock/);
    expect(rejectMessage("nope")).toMatch(/could not/);
  });
});

describe("problemKind and ApiError", () => {
  it("extracts the short name", () => {
    expect(problemKind({ type: "urn:gocart:problem:cart-empty" } as never)).toBe("cart-empty");
    expect(problemKind({ type: "about:blank" } as never)).toBe("");
    expect(problemKind(undefined)).toBe("");
  });
  it("knows when an outcome is unknown", () => {
    expect(new ApiError("x", 0).unknown).toBe(true);
    expect(new ApiError("x", 503).unknown).toBe(true);
    expect(new ApiError("x", 404).unknown).toBe(false);
  });
});

describe("order flow", () => {
  it("places statuses on the happy path", () => {
    expect(progress("awaiting_payment")).toBe(0);
    expect(progress("confirmed")).toBe(1);
    expect(progress("delivered")).toBe(3);
    expect(progress("cancelled")).toBe(-1);
  });
  it("keeps polling while something will still change", () => {
    expect(isSettling({ status: "awaiting_payment" })).toBe(true);
    expect(isSettling({ status: "cancelled", refund_pending: true })).toBe(true);
    expect(isSettling({ status: "delivered" })).toBe(false);
  });
  it("only lets orders be cancelled before they ship", () => {
    expect(canCancel("confirmed")).toBe(true);
    expect(canCancel("shipped")).toBe(false);
    expect(canCancel("delivered")).toBe(false);
  });
});

describe("cart helpers", () => {
  const cart = (extra: Partial<Cart> = {}) => ({ customer_id: "c", items: [{ product_id: "a", name: "A", quantity: 2 }, { product_id: "b", name: "B", quantity: 1 }], ...extra }) as Cart;
  const products = [{ id: "a", price_minor: 1000, stock: 5 }] as Product[];
  it("shows a checked-out cart as empty", () => {
    expect(itemCount(cart())).toBe(3);
    expect(itemCount(cart({ order_id: "ord_1" }))).toBe(0);
    expect(activeLines(undefined)).toEqual([]);
  });
  it("estimates, and says when it cannot", () => {
    expect(estimate(cart().items, products)).toEqual({ minor: 2000, complete: false });
    expect(estimate([cart().items[0]!], products)).toEqual({ minor: 2000, complete: true });
  });
  it("caps quantity by stock and by the service limit", () => {
    expect(maxFor({ stock: 3 })).toBe(3);
    expect(maxFor({ stock: 9999 })).toBe(MAX_QUANTITY);
    expect(maxFor({ stock: 0 })).toBe(0);
  });
});
