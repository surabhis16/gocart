import { useState, type FormEvent } from "react";
import { useCreateProduct, useProducts, useSetStock } from "../../api/hooks";
import { Button, ErrorNote, Loading } from "../../components/ui";
import { formatMoney } from "../../lib/money";

function StockCell({ id, stock }: { id: string; stock: number }) {
  const set = useSetStock();
  const [v, setV] = useState(String(stock));
  return (
    <form className="flex items-center justify-end gap-2" onSubmit={(e) => { e.preventDefault(); const n = Number(v); if (Number.isInteger(n) && n >= 0) set.mutate({ id, stock: n }); }}>
      <input aria-label={`Stock for ${id}`} className="input !w-20 text-right" inputMode="numeric" value={v} onChange={(e) => setV(e.target.value)} />
      <Button variant="secondary" type="submit" busy={set.isPending} disabled={v === String(stock)}>Set</Button>
    </form>
  );
}

export function AdminProducts() {
  const products = useProducts();
  const create = useCreateProduct();
  const [f, setF] = useState({ name: "", description: "", price: "", stock: "" });

  function submit(e: FormEvent) {
    e.preventDefault();
    create.mutate(
      { name: f.name.trim(), description: f.description.trim(), price_minor: Math.round(Number(f.price) * 100), stock: Number(f.stock) },
      { onSuccess: () => setF({ name: "", description: "", price: "", stock: "" }) },
    );
  }
  const field = (k: keyof typeof f, label: string, extra = "") => (
    <div className={extra}>
      <label htmlFor={`p-${k}`} className="text-sm font-medium">{label}</label>
      <input id={`p-${k}`} className="input mt-1" value={f[k]} onChange={(e) => setF({ ...f, [k]: e.target.value })} required={k !== "description"} />
    </div>
  );

  return (
    <div className="space-y-8">
      <form onSubmit={submit} className="panel grid gap-3 p-5 sm:grid-cols-2">
        <h3 className="text-sm font-semibold sm:col-span-2">Add a product</h3>
        {field("name", "Name")}
        {field("description", "Description", "sm:col-span-2")}
        {field("price", "Price in rupees")}
        {field("stock", "Stock")}
        <div className="sm:col-span-2"><Button type="submit" busy={create.isPending}>Add product</Button></div>
        {create.isError && <div className="sm:col-span-2"><ErrorNote error={create.error} /></div>}
      </form>
      {products.isPending ? <Loading /> : products.isError ? <ErrorNote error={products.error} /> : (
        <div className="panel overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="border-b border-ink/30 text-left text-muted"><tr><th className="px-4 py-2 font-medium">Product</th><th className="px-4 py-2 text-right font-medium">Price</th><th className="px-4 py-2 text-right font-medium">Stock</th></tr></thead>
            <tbody className="divide-y divide-dotted divide-ink/30">
              {products.data.map((p) => (
                <tr key={p.id}>
                  <td className="px-4 py-2"><span className="font-semibold">{p.name}</span> <span className="font-mono text-xs text-muted">{p.id}</span></td>
                  <td className="px-4 py-2 text-right font-display font-bold">{formatMoney(p.price_minor, p.currency)}</td>
                  <td className="px-4 py-2"><StockCell key={`${p.id}-${p.stock}`} id={p.id} stock={p.stock} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
