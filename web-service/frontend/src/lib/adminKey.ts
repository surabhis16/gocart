// The admin key lives in memory only. It is never written to localStorage or a
// cookie, so closing the tab forgets it, and it cannot be read by a later visitor
// to the same browser. It is sent to the gateway, which passes it on to the
// service that checks it; the gateway itself never looks at it.
import { useSyncExternalStore } from "react";

let key = "";
const listeners = new Set<() => void>();

export const getAdminKey = () => key;

export function setAdminKey(next: string) {
  key = next;
  listeners.forEach((l) => l());
}

export function useAdminKey(): string {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => key,
  );
}
