import { useState } from "react";
import { Link } from "react-router-dom";
import { useAdminOrders, useCancelOrder, useDeliverOrder, useShipOrder } from "../../api/hooks";
import type { OrderStatus } from "../../api/types";
import { Button, EmptyState, ErrorNote, Loading } from "../../components/ui";
import { StatusBadge } from "../../components/StatusBadge";
import { ALL_STATUSES, canCancel, STATUS_LABEL } from "../../lib/orderFlow";
import { formatMoney } from "../../lib/money";
import { timeAgo } from "../../lib/time";

export function AdminOrders() {
  const [status, setStatus] = useState<OrderStatus | "">("");
  const orders = useAdminOrders(status);
  const ship = useShipOrder();
  const deliver = useDeliverOrder();
  const cancel = useCancelOrder();
  const acting = ship.isPending || deliver.isPending || cancel.isPending;
  const failure = ship.error ?? deliver.error ?? cancel.error;

  return (
    <>
      <div className="mb-4 flex items-center gap-3">
        <label htmlFor="status" className="text-sm font-semibold">Show</label>
        <select id="status" className="input !w-auto" value={status} onChange={(e) => setStatus(e.target.value as OrderStatus | "")}>
          <option value="">All orders</option>
          {ALL_STATUSES.map((s) => <option key={s} value={s}>{STATUS_LABEL[s]}</option>)}
        </select>
      </div>
      {failure && <div className="mb-4"><ErrorNote error={failure} /></div>}
      {orders.isPending ? <Loading label="Loading orders" /> : orders.isError ? <ErrorNote error={orders.error} onRetry={() => orders.refetch()} /> : orders.data.length === 0 ? (
        <EmptyState title="No orders match" />
      ) : (
        <div className="panel overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="border-b border-ink/30 text-left text-muted">
              <tr><th className="px-4 py-2 font-medium">Order</th><th className="px-4 py-2 font-medium">Customer</th><th className="px-4 py-2 font-medium">Status</th><th className="px-4 py-2 text-right font-medium">Total</th><th className="px-4 py-2 font-medium">Placed</th><th className="px-4 py-2" /></tr>
            </thead>
            <tbody className="divide-y divide-dotted divide-ink/30">
              {orders.data.map((o) => (
                <tr key={o.id}>
                  <td className="px-4 py-2"><Link className="font-mono text-brand underline" to={`/admin/orders/${o.id}`}>{o.id}</Link></td>
                  <td className="px-4 py-2 font-mono">{o.customer_id}</td>
                  <td className="px-4 py-2"><StatusBadge status={o.status} /></td>
                  <td className="px-4 py-2 text-right font-display font-bold">{o.total_minor !== undefined ? formatMoney(o.total_minor, o.currency) : "—"}</td>
                  <td className="px-4 py-2 text-muted">{timeAgo(o.placed_at)}</td>
                  <td className="px-4 py-2">
                    <div className="flex justify-end gap-1.5">
                      {o.status === "confirmed" && <Button variant="secondary" disabled={acting} onClick={() => ship.mutate(o.id)}>Ship</Button>}
                      {o.status === "shipped" && <Button variant="secondary" disabled={acting} onClick={() => deliver.mutate(o.id)}>Deliver</Button>}
                      {canCancel(o.status) && <Button variant="ghost" disabled={acting} onClick={() => cancel.mutate(o.id)}>Cancel</Button>}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
