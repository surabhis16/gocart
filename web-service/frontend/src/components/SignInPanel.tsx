import { signInUrl } from "../lib/identity";
import { Link } from "react-router-dom";

const COPY = {
  shop: { title: "Sign in to continue", body: "Your cart and orders belong to your account, so you need to sign in to see them." },
  admin: { title: "Administrators sign in here", body: "The admin console is for the people who run the store." },
  "not-admin": { title: "This is for administrators", body: "" },
} as const;

export function SignInPanel({ next = "/", reason, signedInAs }: { next?: string; reason: keyof typeof COPY; signedInAs?: string }) {
  const c = COPY[reason];
  return (
    <div className="panel mx-auto mt-6 max-w-md p-8 text-center">
      <h1 className="font-display text-3xl font-bold tracking-tight">{c.title}</h1>
      {reason === "not-admin" ? (
        <p className="mt-3 text-muted">
          You are signed in as <span className="font-medium text-ink">{signedInAs}</span>, which is not an administrator account.
        </p>
      ) : (
        <p className="mt-3 text-muted">{c.body}</p>
      )}
      <div className="mt-6 flex justify-center gap-3">
        {reason === "not-admin" ? (
          <Link to="/shop" className="inline-flex items-center rounded-md bg-brand px-4 py-2 font-semibold text-brand-ink">
            Back to the shop
          </Link>
        ) : (
          <a href={signInUrl(next)} className="inline-flex items-center rounded-md bg-brand px-4 py-2 font-semibold text-brand-ink">
            Sign in
          </a>
        )}
      </div>
    </div>
  );
}
