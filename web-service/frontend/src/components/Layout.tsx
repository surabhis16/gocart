import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { signOut as endSession } from "../api/client";
import { useCart } from "../api/hooks";
import { itemCount } from "../lib/cart";
import { meKey, signInUrl, useMe, type SignedIn } from "../lib/identity";
import { Icon } from "./ui";

const link = ({ isActive }: { isActive: boolean }) =>
  `border-b-2 px-1 py-1 text-sm font-semibold transition ${isActive ? "border-brand text-ink" : "border-transparent text-muted hover:text-ink"}`;

function Account({ me }: { me: SignedIn }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const signOut = useMutation({
    mutationFn: endSession,
    onSettled: () => {
      qc.clear();
      qc.setQueryData(meKey, { signed_in: false });
      navigate("/");
    },
  });
  return (
    <details className="relative">
      <summary className="cursor-pointer list-none rounded-md border border-ink/30 px-2.5 py-1 text-sm font-semibold hover:border-ink">
        {me.name.split(" ")[0]}
      </summary>
      <div className="panel absolute right-0 z-10 mt-2 w-72 p-4">
        <p className="font-semibold">{me.name}</p>
        <p className="text-sm text-muted">{me.email}</p>
        <p className="mt-2 text-xs text-muted">
          Customer id <span className="font-mono text-ink">{me.customer_id}</span>
          {me.role === "admin" && " · administrator"}
        </p>
        <button
          className="mt-3 rounded-md border border-ink/30 px-3 py-1.5 text-sm font-semibold hover:border-ink"
          onClick={() => signOut.mutate()}
          disabled={signOut.isPending}
        >
          Sign out
        </button>
      </div>
    </details>
  );
}

export function Layout() {
  const me = useMe();
  const person = me.data?.signed_in ? me.data : null;
  const cart = useCart(person?.customer_id ?? "");
  const count = person ? itemCount(cart.data) : 0;
  const here = useLocation();

  return (
    <div className="flex min-h-screen flex-col">
      <header className="sticky top-0 z-20 border-b border-ink/20 bg-canvas/95 backdrop-blur">
        <div className="mx-auto flex min-h-16 max-w-6xl flex-wrap items-center gap-x-6 gap-y-1 px-4 py-2">
          <Link to="/" className="flex items-center gap-2 font-display text-2xl font-extrabold tracking-tighter" aria-label="GoCart, home">
            {/* a price tag: the punched hole is the only decoration */}
            <span aria-hidden="true" className="relative h-5 w-5 rounded-sm bg-brand [clip-path:polygon(0_0,70%_0,100%_30%,100%_100%,0_100%)]">
              <span className="absolute left-1 top-1 h-1.5 w-1.5 rounded-full bg-canvas" />
            </span>
            gocart
          </Link>
          <nav className="order-last flex w-full items-center gap-5 sm:order-none sm:w-auto" aria-label="Main">
            <NavLink to="/" end className={link}>
              Shop
            </NavLink>
            <NavLink to="/orders" className={link}>
              Orders
            </NavLink>
            <NavLink to="/cart" className={link}>
              Cart
              {count > 0 && (
                <span className="ml-1.5 rounded-sm bg-brand px-1.5 py-0.5 text-[11px] font-bold text-brand-ink" aria-label={`${count} items`}>
                  {count}
                </span>
              )}
            </NavLink>
          </nav>
          <div className="ml-auto flex items-center gap-2">
            {person?.role === "admin" && (
              <NavLink to="/admin" className={link}>
                <span className="flex items-center gap-1.5">
                  <Icon name="shield" className="h-4 w-4" />
                  <span className="hidden sm:inline">Admin</span>
                  <span className="sr-only sm:hidden">Admin</span>
                </span>
              </NavLink>
            )}
            {person ? (
              <Account me={person} />
            ) : (
              me.isSuccess && (
                <a
                  href={signInUrl(here.pathname === "/" ? "/shop" : here.pathname + here.search)}
                  className="rounded-md bg-brand px-3 py-1.5 text-sm font-semibold text-brand-ink hover:opacity-90"
                >
                  Sign in
                </a>
              )
            )}
          </div>
        </div>
      </header>
      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-8">
        <Outlet />
      </main>
      <footer className="border-t border-ink/20 py-6 text-center text-sm text-muted">
        A demo store. Nothing is charged and nothing ships.
      </footer>
    </div>
  );
}
