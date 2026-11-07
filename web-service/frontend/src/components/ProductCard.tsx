import type { Product } from "../api/types";
import { formatMoney } from "../lib/money";
import { maxFor } from "../lib/cart";
import { Button, Icon } from "./ui";
import { ProductArt } from "./ProductArt";

function Stock({ stock }: { stock: number }) {
  if (stock <= 0) return <span className="text-sm font-medium text-bad">Sold out</span>;
  if (stock <= 5) return <span className="text-sm font-medium text-warn">Only {stock} left</span>;
  return <span className="text-sm text-muted">{stock} in stock</span>;
}

interface Props {
  product: Product;
  /** How many are already in the cart. */
  inCart: number;
  busy: boolean;
  onSet: (quantity: number) => void;
}

export function ProductCard({ product: p, inCart, busy, onSet }: Props) {
  const max = maxFor(p);
  const soldOut = p.stock <= 0;
  return (
    <article className={`panel flex flex-col overflow-hidden transition hover:border-ink ${soldOut ? "opacity-70" : ""}`}>
      <ProductArt id={p.id} className="aspect-[8/5] w-full border-b border-ink/20" />
      <div className="flex flex-1 flex-col p-4">
        <h2 className="text-lg font-semibold leading-snug">{p.name}</h2>
        <p className="mt-1 flex-1 text-sm leading-relaxed text-muted">{p.description}</p>
        <div className="mt-3">
          <Stock stock={p.stock} />
        </div>
        <div className="mt-3 flex items-center justify-between gap-3 border-t border-dotted border-ink/40 pt-3">
          <span className="font-display text-2xl font-bold tracking-tight text-brand">{formatMoney(p.price_minor, p.currency)}</span>
          {inCart === 0 ? (
            <Button disabled={max === 0} busy={busy} onClick={() => onSet(1)} aria-label={`Add ${p.name} to cart`}>
              Add to cart
            </Button>
          ) : (
            <div className="flex items-center" role="group" aria-label={`${p.name} quantity`}>
              <Button variant="secondary" className="!rounded-r-none !px-2.5" disabled={busy} onClick={() => onSet(inCart - 1)} aria-label="Remove one">
                <Icon name="minus" className="h-4 w-4" />
              </Button>
              <span className="flex h-[38px] w-10 items-center justify-center border-y border-ink/30 bg-surface text-sm font-semibold">{inCart}</span>
              <Button variant="secondary" className="!rounded-l-none !px-2.5" disabled={busy || inCart >= max} onClick={() => onSet(inCart + 1)} aria-label="Add one more">
                <Icon name="plus" className="h-4 w-4" />
              </Button>
            </div>
          )}
        </div>
      </div>
    </article>
  );
}
