// Who is signed in. The gateway owns this: the browser holds a signed cookie it
// cannot read, and asks GET /api/me who it belongs to. Nothing about identity is
// kept in localStorage, and the page never sees a token.
import { createContext, useContext } from "react";
import { useQuery } from "@tanstack/react-query";
import { request } from "../api/client";

export interface SignedOut {
  signed_in: false;
  /** A line from the deployment, such as demo accounts. Shown on the landing page only. */
  hint?: string;
}

export interface SignedIn {
  signed_in: true;
  customer_id: string;
  name: string;
  email: string;
  role: "customer" | "admin";
}

export type Me = SignedOut | SignedIn;

export const meKey = ["me"] as const;

export function useMe() {
  return useQuery({
    queryKey: meKey,
    queryFn: () => request<Me>("/me"),
    staleTime: 60_000,
    retry: false,
  });
}

/** Where /auth/login sends people after they sign in. Only a path on this site is honoured. */
export function signInUrl(next: string): string {
  return `/auth/login?next=${encodeURIComponent(next)}`;
}

const SignedInContext = createContext<SignedIn | null>(null);
export const SignedInProvider = SignedInContext.Provider;

/** The signed-in person. Only valid below RequireSignIn, which is the only thing that provides it. */
export function useSignedIn(): SignedIn {
  const me = useContext(SignedInContext);
  if (!me) throw new Error("useSignedIn used outside RequireSignIn");
  return me;
}

/** The customer id the services know this person by. */
export function useCustomerId(): string {
  return useSignedIn().customer_id;
}
