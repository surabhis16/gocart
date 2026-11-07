import { Link } from "react-router-dom";
import { useMyOrders } from "../api/hooks";
import { EmptyState, ErrorNote, Loading, PageTitle } from "../components/ui";
import { StatusBadge } from "../components/StatusBadge";
import { useCustomerId } from "../lib/identity";
import { formatMoney } from "../lib/money";
import { timeAgo } from "../lib/time";

export function OrdersPage() {
  const customer = useCustomerId();
  const orders = useMyOrders(customer);

  if (orders.isPending) return <Loading label="Loading your orders" />;
  if (orders.isError) return <ErrorNote error={orders.error} onRetry={() => orders.refetch()} />;

  return (
    <>
      <PageTitle title="Orders" />
      {orders.data.length === 0 ? (
        <EmptyState title="No orders yet">
          <Link to="/" className="font-semibold text-brand underline">Find something in the shop</Link>
        </EmptyState>
      ) : (
        <ul className="panel divide-y divide-dotted divide-ink/30">
          {orders.data.map((o) => (
            <li key={o.id}>
              <Link to={`/orders/${o.id}`} className="flex flex-wrap items-center gap-x-6 gap-y-1 px-5 py-4 hover:bg-sunken">
                <span className="min-w-0 flex-1">
                  <span className="block truncate font-mono text-sm">{o.id}</span>
                  <span className="text-sm text-muted">
                    {o.items.reduce((n, i) => n + i.quantity, 0)} items, {timeAgo(o.placed_at)}
                  </span>
                </span>
                <StatusBadge status={o.status} />
                <span className="w-28 text-right font-display text-lg font-bold">{o.total_minor !== undefined ? formatMoney(o.total_minor, o.currency) : "—"}</span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </>
  );
}
