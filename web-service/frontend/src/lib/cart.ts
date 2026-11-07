import type { Cart, CartLine, Product } from "../api/types";

export const MAX_QUANTITY = 100;

/** A checked-out cart is kept by the backend so a repeated checkout finds its order; to a shopper it is empty. */
export function activeLines(cart?: Cart): CartLine[] {
  return cart && !cart.order_id ? cart.items : [];
}

export function itemCount(cart?: Cart): number {
  return activeLines(cart).reduce((n, l) => n + l.quantity, 0);
}

/** The most of this product the shopper can put in the cart. Stock is only reserved at checkout, so it is a guide, not a promise. */
export function maxFor(p: Pick<Product, "stock">): number {
  return Math.max(0, Math.min(p.stock, MAX_QUANTITY));
}

/**
 * An estimate for display. The cart holds no prices; the real ones are quoted
 * by the Product service when stock is reserved at checkout, so this is labelled
 * as an estimate wherever it is shown.
 */
export function estimate(lines: CartLine[], products: Product[]): { minor: number; complete: boolean } {
  const byId = new Map(products.map((p) => [p.id, p]));
  let minor = 0;
  let complete = true;
  for (const l of lines) {
    const p = byId.get(l.product_id);
    if (p) minor += p.price_minor * l.quantity;
    else complete = false;
  }
  return { minor, complete };
}
