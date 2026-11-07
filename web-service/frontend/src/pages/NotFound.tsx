import { Link } from "react-router-dom";
import { EmptyState } from "../components/ui";

export function NotFound() {
  return <EmptyState title="That page does not exist"><Link to="/" className="font-semibold text-brand underline">Back to the shop</Link></EmptyState>;
}
