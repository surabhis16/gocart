import { useState, type FormEvent } from "react";
import { NavLink, Outlet } from "react-router-dom";
import { checkAdminKey } from "../../api/hooks";
import { setAdminKey, useAdminKey } from "../../lib/adminKey";
import { Button, Icon, Notice, PageTitle } from "../../components/ui";

const tab = ({ isActive }: { isActive: boolean }) =>
  `border-b-2 px-1 pb-2 text-sm font-semibold ${isActive ? "border-ink text-ink" : "border-transparent text-muted hover:text-ink"}`;

function Gate() {
  const [value, setValue] = useState("");
  const [state, setState] = useState<"idle" | "checking" | "invalid" | "unreachable">("idle");

  async function submit(e: FormEvent) {
    e.preventDefault();
    setState("checking");
    try {
      const r = await checkAdminKey(value.trim());
      if (r === "valid") setAdminKey(value.trim());
      else setState(r);
    } catch {
      setState("unreachable");
    }
  }

  return (
    <div className="mx-auto max-w-md">
      <PageTitle title="Admin" />
      <form onSubmit={submit} className="panel space-y-3 p-5">
        <label htmlFor="admin-key" className="flex items-center gap-2 text-sm font-semibold">
          <Icon name="shield" className="h-4 w-4" /> Admin key
        </label>
        <input id="admin-key" type="password" autoComplete="off" value={value} onChange={(e) => setValue(e.target.value)} className="input font-mono" />
        <p className="text-xs text-muted">The key is kept in this tab's memory only. Closing the tab forgets it. The default for the local stack is in the README.</p>
        <Button type="submit" busy={state === "checking"} disabled={!value.trim()}>Unlock</Button>
        {state === "invalid" && <Notice tone="bad" title="That key was not accepted">Check it and try again.</Notice>}
        {state === "unreachable" && <Notice tone="warn" title="Could not reach the order service">Nothing was checked. Try again in a moment.</Notice>}
      </form>
    </div>
  );
}

export function AdminLayout() {
  const key = useAdminKey();
  if (!key) return <Gate />;
  return (
    <>
      <PageTitle title="Admin">
        <Button variant="secondary" onClick={() => setAdminKey("")}>Lock</Button>
      </PageTitle>
      <nav className="mb-6 flex gap-6 border-b border-ink/20" aria-label="Admin">
        <NavLink to="/admin" end className={tab}>Orders</NavLink>
        <NavLink to="/admin/products" className={tab}>Products</NavLink>
        <NavLink to="/admin/system" className={tab}>System</NavLink>
      </nav>
      <Outlet />
    </>
  );
}
