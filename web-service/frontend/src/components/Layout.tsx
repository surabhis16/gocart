import { useState, type FormEvent } from "react";
import { Link, NavLink, Outlet } from "react-router-dom";
import { useCart } from "../api/hooks";
import { itemCount } from "../lib/cart";
import { setCustomerId, useCustomerId } from "../lib/identity";
import { Icon } from "./ui";

const link = ({ isActive }: { isActive: boolean }) =>
  `border-b-2 px-1 py-1 text-sm font-semibold transition ${isActive ? "border-brand text-ink" : "border-transparent text-muted hover:text-ink"}`;

function Identity() {
  const customer = useCustomerId();
  const [error, setError] = useState("");

  function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const next = String(new FormData(e.currentTarget).get("id") ?? "").trim();
    if (!setCustomerId(next)) {
      setError("Use 1–64 letters, digits, dots, dashes or underscores.");
      return;
    }
    setError("");
    (e.currentTarget.closest("details") as HTMLDetailsElement | null)?.removeAttribute("open");
  }

  return (
    <details className="relative">
      <summary className="cursor-pointer list-none rounded-md border border-ink/30 px-2.5 py-1 font-mono text-xs text-muted hover:border-ink hover:text-ink">
        {customer}
      </summary>
      <form onSubmit={submit} className="panel absolute right-0 z-10 mt-2 w-72 p-4">
        <label htmlFor="customer-id" className="text-sm font-medium">
          Shopping as
        </label>
        <p className="mb-2 mt-0.5 text-xs text-muted">
          There are no accounts in this demo. Your cart and orders belong to this id, kept in this browser. Use another
          id to be another shopper.
        </p>
        <input id="customer-id" name="id" defaultValue={customer} key={customer} className="input font-mono" autoComplete="off" />
        {error && <p className="mt-1 text-xs text-bad">{error}</p>}
        <button className="mt-3 rounded-md bg-brand px-3 py-1.5 text-sm font-semibold text-brand-ink">Switch</button>
      </form>
    </details>
  );
}

export function Layout() {
  const customer = useCustomerId();
  const cart = useCart(customer);
  const count = itemCount(cart.data);

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
            <Identity />
            <NavLink to="/admin" className={link}>
              <span className="flex items-center gap-1.5">
                <Icon name="shield" className="h-4 w-4" />
                <span className="hidden sm:inline">Admin</span>
                <span className="sr-only sm:hidden">Admin</span>
              </span>
            </NavLink>
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
