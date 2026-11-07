import { Link, useParams } from "react-router-dom";
import { useCharge, useOrder, useOrderEvents } from "../../api/hooks";
import { ApiError } from "../../api/client";
import { ErrorNote, Loading } from "../../components/ui";
import { EventTimeline } from "../../components/EventTimeline";
import { StatusBadge } from "../../components/StatusBadge";
import { Receipt } from "../OrderDetail";
import { formatMoney } from "../../lib/money";

export function AdminOrder() {
  const { id = "" } = useParams();
  const order = useOrder(id);
  const events = useOrderEvents(id);
  const charge = useCharge(id);

  return (
    <div className="space-y-8">
      <Link to="/admin" className="text-sm font-semibold text-brand underline">All orders</Link>
      {order.isPending ? <Loading /> : order.isError ? <ErrorNote error={order.error} /> : (
        <div className="grid gap-10 lg:grid-cols-[1fr_28rem]">
          <section className="space-y-6">
            <div className="flex items-center gap-3">
              <h2 className="font-mono text-xl">{order.data.id}</h2>
              <StatusBadge status={order.data.status} />
            </div>
            <div className="panel p-5">
              <h3 className="mb-2 text-sm font-semibold">Charge</h3>
              {charge.isPending ? <Loading /> : charge.isError ? (
                charge.error instanceof ApiError && charge.error.status === 404
                  ? <p className="text-sm text-muted">No charge exists. This order never reached payment.</p>
                  : <ErrorNote error={charge.error} />
              ) : (
                <dl className="space-y-1 text-sm">
                  <div className="leader"><dt className="text-muted">Amount</dt><dd className="font-bold">{formatMoney(charge.data.amount_minor, charge.data.currency)}</dd></div>
                  <div className="leader"><dt className="text-muted">State</dt><dd>{charge.data.status}</dd></div>
                  <div className="leader"><dt className="text-muted">Reference</dt><dd className="font-mono">{charge.data.reference ?? "none"}</dd></div>
                </dl>
              )}
            </div>
            <div>
              <h3 className="mb-3 text-sm font-semibold">Event log</h3>
              <p className="mb-3 text-sm text-muted">The order's state is computed from these facts, in this order. Nothing here is ever edited.</p>
              {events.isPending ? <Loading /> : events.isError ? <ErrorNote error={events.error} /> : <EventTimeline events={events.data} />}
            </div>
          </section>
          <Receipt order={order.data} />
        </div>
      )}
    </div>
  );
}
