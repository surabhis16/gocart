import { useMemo, useState } from "react";
import { useCart, useProducts, useRemoveItem, useSetItem } from "../api/hooks";
import { ErrorNote, Loading, PageTitle, EmptyState } from "../components/ui";
import { ProductCard } from "../components/ProductCard";
import { activeLines } from "../lib/cart";
import { useCustomerId } from "../lib/identity";

export function Shop() {
  const customer = useCustomerId();
  const products = useProducts();
  const cart = useCart(customer);
  const setItem = useSetItem(customer);
  const removeItem = useRemoveItem(customer);
  const [query, setQuery] = useState("");

  const lines = activeLines(cart.data);
  const inCart = useMemo(() => new Map(lines.map((l) => [l.product_id, l.quantity])), [lines]);
  const shown = useMemo(() => {
    const q = query.trim().toLowerCase();
    const all = products.data ?? [];
    return q ? all.filter((p) => `${p.name} ${p.description}`.toLowerCase().includes(q)) : all;
  }, [products.data, query]);

  if (products.isPending) return <Loading label="Loading the catalogue" />;
  if (products.isError) return <ErrorNote error={products.error} onRetry={() => products.refetch()} />;

  const change = (productId: string, quantity: number) =>
    quantity <= 0 ? removeItem.mutate(productId) : setItem.mutate({ productId, quantity });
  const failure = setItem.error ?? removeItem.error;

  return (
    <>
      <PageTitle title="Shop">
        <input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search products"
          aria-label="Search products"
          className="input !w-64"
        />
      </PageTitle>
      {failure && (
        <div className="mb-4">
          <ErrorNote error={failure} />
        </div>
      )}
      {shown.length === 0 ? (
        <EmptyState title={query ? "Nothing matches that search" : "The catalogue is empty"} />
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {shown.map((p) => (
            <ProductCard
              key={p.id}
              product={p}
              inCart={inCart.get(p.id) ?? 0}
              busy={(setItem.isPending && setItem.variables?.productId === p.id) || (removeItem.isPending && removeItem.variables === p.id)}
              onSet={(q) => change(p.id, q)}
            />
          ))}
        </div>
      )}
    </>
  );
}
