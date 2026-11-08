import { NavLink, Outlet } from "react-router-dom";
import { RequireSignIn } from "../../components/RequireSignIn";
import { PageTitle } from "../../components/ui";

const tab = ({ isActive }: { isActive: boolean }) =>
  `border-b-2 px-1 pb-2 text-sm font-semibold ${isActive ? "border-ink text-ink" : "border-transparent text-muted hover:text-ink"}`;

/**
 * The console is only for administrators, and who is one is decided by the
 * gateway when someone signs in. Hiding it here is a courtesy: the gateway
 * refuses every admin call from anyone else.
 */
export function AdminLayout() {
  return (
    <RequireSignIn admin>
      <PageTitle title="Admin" />
      <nav className="mb-6 flex gap-6 border-b border-ink/20" aria-label="Admin">
        <NavLink to="/admin" end className={tab}>Orders</NavLink>
        <NavLink to="/admin/products" className={tab}>Products</NavLink>
        <NavLink to="/admin/system" className={tab}>System</NavLink>
      </nav>
      <Outlet />
    </RequireSignIn>
  );
}
