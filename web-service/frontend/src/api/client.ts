import type { Problem } from "./types";

/** What went wrong, in the terms the screens care about. */
export class ApiError extends Error {
  constructor(
    message: string,
    /** HTTP status, or 0 when no response arrived at all. */
    readonly status: number,
    readonly problem?: Problem,
    readonly requestId?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }

  /** The short name of the problem type, e.g. "payment-declined". */
  get kind(): string {
    return problemKind(this.problem);
  }

  /** True when nothing is known about whether the request took effect. */
  get unknown(): boolean {
    return this.status === 0 || this.status >= 500 || this.status === 429 || this.status === 408;
  }
}

const PREFIX = "urn:gocart:problem:";

export function problemKind(p?: Problem): string {
  const t = p?.type;
  return typeof t === "string" && t.startsWith(PREFIX) ? t.slice(PREFIX.length) : "";
}

export interface RawResponse<T = unknown> {
  status: number;
  body: T;
  requestId?: string;
}

interface Options {
  method?: string;
  body?: unknown;
  headers?: Record<string, string>;
  signal?: AbortSignal;
}

/**
 * Makes a request and returns whatever status came back, without judging it.
 * Checkout needs this: 201, 200, 202, 402 and 409 are all meaningful answers.
 * Everything else should use request(), which throws on a non-2xx.
 */
export async function requestRaw<T = unknown>(path: string, o: Options = {}): Promise<RawResponse<T>> {
  const headers: Record<string, string> = { Accept: "application/json", ...o.headers };
  if (o.body !== undefined) headers["Content-Type"] = "application/json";
  // Marks the call as this app's own. The gateway refuses state-changing calls
  // without it, which is what stops another website from making them.
  headers["X-Requested-With"] = "gocart";

  let res: Response;
  try {
    res = await fetch(`/api${path}`, {
      method: o.method ?? "GET",
      headers,
      body: o.body === undefined ? undefined : JSON.stringify(o.body),
      signal: o.signal,
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw new ApiError("The server could not be reached.", 0);
  }
  const requestId = res.headers.get("X-Request-ID") ?? undefined;
  const text = await res.text();
  let body: unknown = undefined;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = undefined; // an HTML error page from a proxy, say; the status still tells the story
    }
  }
  return { status: res.status, body: body as T, requestId };
}

/** Like requestRaw, but a non-2xx response becomes an ApiError. */
export async function request<T>(path: string, o: Options = {}): Promise<T> {
  const res = await requestRaw<T>(path, o);
  if (res.status >= 200 && res.status < 300) return res.body;
  throw toError(res);
}

export function toError(res: RawResponse): ApiError {
  const p = res.body as Problem | undefined;
  const isProblem = p && typeof p === "object" && typeof p.type === "string";
  const message = isProblem ? (p.detail ?? p.title ?? `Request failed (${res.status})`) : `Request failed (${res.status})`;
  return new ApiError(message, res.status, isProblem ? p : undefined, res.requestId);
}

/** Ends the session. It lives outside /api because the gateway, not a service, owns it. */
export async function signOut(): Promise<void> {
  const res = await fetch("/auth/logout", { method: "POST", headers: { "X-Requested-With": "gocart" } });
  if (!res.ok) throw new ApiError("Could not sign out.", res.status);
}
