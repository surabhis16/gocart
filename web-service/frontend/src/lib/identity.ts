// There are no logins yet. A visitor is whoever they say they are: a customer
// id is picked on the first visit and kept in this browser. The header says so.
// (The services behave the same way: anyone who knows an id can read its cart.)
import { useSyncExternalStore } from "react";

const STORAGE_KEY = "gocart.customer";
export const CUSTOMER_ID = /^[A-Za-z0-9._-]{1,64}$/;

const listeners = new Set<() => void>();

function read(): string {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved && CUSTOMER_ID.test(saved)) return saved;
  } catch {
    /* private mode or blocked storage: fall through to a session-only id */
  }
  return "";
}

let current = read() || remember(newId());

/** Keeps a freshly made id, so a reload is the same shopper and not a new one. */
function remember(id: string): string {
  try {
    localStorage.setItem(STORAGE_KEY, id);
  } catch {
    /* session-only id */
  }
  return id;
}

function newId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(4));
  return "cust_" + Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

export function getCustomerId(): string {
  return current;
}

/** Returns false, and changes nothing, if the id is not acceptable. */
export function setCustomerId(id: string): boolean {
  if (!CUSTOMER_ID.test(id)) return false;
  current = id;
  try {
    localStorage.setItem(STORAGE_KEY, id);
  } catch {
    /* keep it for this session only */
  }
  listeners.forEach((l) => l());
  return true;
}

export function useCustomerId(): string {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => current,
  );
}
