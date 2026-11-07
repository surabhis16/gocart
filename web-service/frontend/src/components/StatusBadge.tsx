import type { OrderStatus } from "../api/types";
import { STATUS_LABEL, STATUS_TONE } from "../lib/orderFlow";
import { Badge } from "./ui";

export function StatusBadge({ status }: { status: OrderStatus }) {
  return <Badge tone={STATUS_TONE[status]}>{STATUS_LABEL[status]}</Badge>;
}
