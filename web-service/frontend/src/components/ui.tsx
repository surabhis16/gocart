import type { ButtonHTMLAttributes, ReactNode } from "react";
import { ApiError } from "../api/client";
import type { Tone } from "../lib/orderFlow";

type Variant = "primary" | "secondary" | "danger" | "ghost";

const VARIANT: Record<Variant, string> = {
  primary: "border border-brand bg-brand text-brand-ink hover:brightness-110",
  secondary: "border border-ink/30 bg-surface text-ink hover:border-ink",
  danger: "border border-bad/50 bg-surface text-bad hover:border-bad hover:bg-bad/5",
  ghost: "border border-transparent text-muted hover:text-ink",
};

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  busy?: boolean;
}

export function Button({ variant = "primary", busy, disabled, className = "", children, ...rest }: ButtonProps) {
  return (
    <button
      {...rest}
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      className={`inline-flex items-center justify-center gap-2 rounded-md px-3.5 py-2 text-sm font-semibold transition disabled:cursor-not-allowed disabled:opacity-50 ${VARIANT[variant]} ${className}`}
    >
      {busy && <Spinner className="h-4 w-4" />}
      {children}
    </button>
  );
}

export function Spinner({ className = "h-5 w-5" }: { className?: string }) {
  return (
    <svg className={`animate-spin ${className}`} viewBox="0 0 24 24" fill="none" role="status" aria-label="Loading">
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth="3" opacity="0.25" />
      <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
    </svg>
  );
}

const TONE: Record<Tone, { text: string; glyph: string }> = {
  ok: { text: "text-ok", glyph: "●" },
  warn: { text: "text-warn", glyph: "◐" },
  bad: { text: "text-bad", glyph: "✕" },
  info: { text: "text-info", glyph: "▲" },
  neutral: { text: "text-muted", glyph: "○" },
};

/** A status. The glyph carries the meaning when colour cannot: filled, half, cross, triangle, empty. */
export function Badge({ tone = "neutral", children }: { tone?: Tone; children: ReactNode }) {
  const t = TONE[tone];
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-sm border border-current/30 px-2 py-0.5 text-xs font-semibold ${t.text}`}>
      <span aria-hidden="true" className="text-[9px] leading-none">
        {t.glyph}
      </span>
      {children}
    </span>
  );
}

export function PageTitle({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="mb-8 flex flex-wrap items-end justify-between gap-4">
      <h1 className="text-4xl font-bold leading-none tracking-tight sm:text-5xl">{title}</h1>
      {children}
    </div>
  );
}

export function Loading({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-3 py-16 text-muted">
      <Spinner />
      <span>{label}…</span>
    </div>
  );
}

export function EmptyState({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="panel px-6 py-14 text-center">
      <p className="font-display text-xl font-semibold">{title}</p>
      {children && <div className="mt-2 text-sm text-muted">{children}</div>}
    </div>
  );
}

/** Shows what the backend said, in its own words, with the id to quote when reporting it. */
export function ErrorNote({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const e = error instanceof ApiError ? error : undefined;
  const message = e ? e.message : error instanceof Error ? error.message : "Something went wrong.";
  return (
    <div role="alert" className="rounded-md border border-bad/50 bg-bad/5 px-4 py-3 text-sm">
      <p className="font-medium text-bad">{e?.problem?.title ?? "Something went wrong"}</p>
      <p className="mt-0.5 text-ink">{message}</p>
      {e?.requestId && (
        <p className="mt-1 text-xs text-muted">
          Request ID <code className="font-mono">{e.requestId}</code>
        </p>
      )}
      {onRetry && (
        <Button variant="secondary" className="mt-3" onClick={onRetry}>
          Try again
        </Button>
      )}
    </div>
  );
}

export function Notice({ tone, title, children }: { tone: Tone; title: string; children?: ReactNode }) {
  const box: Record<Tone, string> = {
    ok: "border-ok/50 bg-ok/5",
    warn: "border-warn/50 bg-warn/5",
    bad: "border-bad/50 bg-bad/5",
    info: "border-info/50 bg-info/5",
    neutral: "border-ink/20 bg-sunken",
  };
  return (
    <div role="status" className={`rounded-md border px-4 py-3 text-sm ${box[tone]}`}>
      <p className="font-medium">{title}</p>
      {children && <div className="mt-0.5 text-muted">{children}</div>}
    </div>
  );
}

export function Icon({ name, className = "h-5 w-5" }: { name: "cart" | "box" | "shield" | "plus" | "minus" | "trash" | "check"; className?: string }) {
  const paths: Record<string, string> = {
    cart: "M3 4h2l2 11h11l2-8H6M9 20a1 1 0 1 0 0-2 1 1 0 0 0 0 2zm9 0a1 1 0 1 0 0-2 1 1 0 0 0 0 2z",
    box: "M21 8l-9-5-9 5m18 0v8l-9 5m9-13l-9 5m0 8l-9-5V8m9 13v-8M3 8l9 5",
    shield: "M12 3l8 3v6c0 4.5-3.2 8-8 9-4.8-1-8-4.5-8-9V6l8-3z",
    plus: "M12 5v14M5 12h14",
    minus: "M5 12h14",
    trash: "M4 7h16M10 11v6m4-6v6M6 7l1 12a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-12M9 7V4h6v3",
    check: "M5 12l4 4 10-10",
  };
  return (
    <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d={paths[name]} />
    </svg>
  );
}
