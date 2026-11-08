import { useLocation } from "react-router-dom";
import { Loading, ErrorNote } from "./ui";
import { SignInPanel } from "./SignInPanel";
import { SignedInProvider, useMe } from "../lib/identity";
import type { ReactNode } from "react";

/**
 * Shows its children only to someone who is signed in, and gives them the
 * person to ask for. Everyone else sees a plain invitation to sign in, with the
 * page they wanted remembered, so signing in lands them where they were going.
 */
export function RequireSignIn({ children, admin = false }: { children: ReactNode; admin?: boolean }) {
  const me = useMe();
  const here = useLocation();
  if (me.isPending) return <Loading label="Checking who is signed in" />;
  if (me.isError) return <ErrorNote error={me.error} onRetry={() => me.refetch()} />;
  if (!me.data.signed_in) return <SignInPanel next={here.pathname + here.search} reason={admin ? "admin" : "shop"} />;
  if (admin && me.data.role !== "admin") return <SignInPanel signedInAs={me.data.email} reason="not-admin" />;
  return <SignedInProvider value={me.data}>{children}</SignedInProvider>;
}
