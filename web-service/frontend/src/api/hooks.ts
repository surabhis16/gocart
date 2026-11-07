import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, request, requestRaw, toError } from "./client";
import type { Cart, Charge, NewProduct, Order, OrderEvent, OrderStatus, Product, ServiceStatus } from "./types";
import { interpretCheckout, interpretFailure, type CheckoutOutcome } from "../lib/checkout";
import { isSettling } from "../lib/orderFlow";

const enc = encodeURIComponent;

export const keys = {
  products: ["products"] as const,
  cart: (c: string) => ["cart", c] as const,
  orders: (c: string) => ["orders", "customer", c] as const,
  order: (id: string) => ["order", id] as const,
  adminOrders: (status: string) => ["orders", "admin", status] as const,
  events: (id: string) => ["events", id] as const,
  charge: (id: string) => ["charge", id] as const,
  status: ["status"] as const,
};

// ---- catalogue -------------------------------------------------------------

export function useProducts() {
  return useQuery({
    queryKey: keys.products,
    queryFn: async () => (await request<{ items: Product[] }>("/products")).items,
    refetchInterval: 15_000, // stock changes under the customer; keep the shelf honest
  });
}

// ---- cart ------------------------------------------------------------------

export function useCart(customer: string) {
  return useQuery({ queryKey: keys.cart(customer), queryFn: () => request<Cart>(`/carts/${enc(customer)}`) });
}

export function useSetItem(customer: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { productId: string; quantity: number }) =>
      request<Cart>(`/carts/${enc(customer)}/items/${enc(v.productId)}`, { method: "PUT", body: { quantity: v.quantity } }),
    onSuccess: (cart) => qc.setQueryData(keys.cart(customer), cart),
  });
}

export function useRemoveItem(customer: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (productId: string) =>
      request<Cart>(`/carts/${enc(customer)}/items/${enc(productId)}`, { method: "DELETE" }),
    onSuccess: (cart) => qc.setQueryData(keys.cart(customer), cart),
  });
}

export function useClearCart(customer: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => request<void>(`/carts/${enc(customer)}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.cart(customer) }),
  });
}

/**
 * Checkout resolves with an outcome for every answer the backend can give and
 * only throws for things that are bugs. Repeating the call is always safe: the
 * Cart service derives the Idempotency-Key from the cart, not from the click.
 */
export function useCheckout(customer: string) {
  const qc = useQueryClient();
  return useMutation<CheckoutOutcome, Error, string>({
    mutationFn: async (paymentToken) => {
      try {
        const res = await requestRaw(`/carts/${enc(customer)}/checkout`, {
          method: "POST",
          body: { payment_token: paymentToken },
        });
        return interpretCheckout(res);
      } catch (e) {
        return interpretFailure(e);
      }
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: keys.cart(customer) });
      qc.invalidateQueries({ queryKey: keys.products });
      qc.invalidateQueries({ queryKey: keys.orders(customer) });
    },
  });
}

// ---- orders ----------------------------------------------------------------

export function useMyOrders(customer: string) {
  return useQuery({
    queryKey: keys.orders(customer),
    queryFn: async () => (await request<{ items: Order[] }>(`/orders?customer_id=${enc(customer)}&limit=50`)).items,
    refetchInterval: (q) => (q.state.data?.some(isSettling) ? 2_000 : 15_000),
  });
}

/** Polls while the order is still changing by itself, and stops when it is settled. */
export function useOrder(id: string) {
  return useQuery({
    queryKey: keys.order(id),
    queryFn: () => request<Order>(`/orders/${enc(id)}`),
    refetchInterval: (q) => (q.state.data && isSettling(q.state.data) ? 1_500 : false),
    retry: (count, e) => !(e instanceof ApiError && e.status === 404) && count < 2,
  });
}

function useOrderAction(action: "cancel" | "ship" | "deliver", admin: boolean) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => request<Order>(`/orders/${enc(id)}/${action}`, { method: "POST", admin }),
    onSuccess: (order) => {
      qc.setQueryData(keys.order(order.id), order);
      qc.invalidateQueries({ queryKey: ["orders"] });
      qc.invalidateQueries({ queryKey: keys.events(order.id) });
      qc.invalidateQueries({ queryKey: keys.products });
    },
  });
}

export const useCancelOrder = () => useOrderAction("cancel", false);
export const useShipOrder = () => useOrderAction("ship", true);
export const useDeliverOrder = () => useOrderAction("deliver", true);

// ---- admin -----------------------------------------------------------------

export function useAdminOrders(status: OrderStatus | "") {
  return useQuery({
    queryKey: keys.adminOrders(status),
    queryFn: async () =>
      (await request<{ items: Order[] }>(`/orders?limit=100${status ? `&status=${status}` : ""}`)).items,
    refetchInterval: 4_000,
  });
}

export function useOrderEvents(id: string) {
  return useQuery({
    queryKey: keys.events(id),
    queryFn: async () => (await request<{ events: OrderEvent[] }>(`/orders/${enc(id)}/events`, { admin: true })).events,
    refetchInterval: 3_000,
  });
}

export function useCharge(orderId: string) {
  return useQuery({
    queryKey: keys.charge(orderId),
    queryFn: () => request<Charge>(`/charges/${enc(orderId)}`, { admin: true }),
    retry: false, // an order that never reached payment has no charge: 404 is an answer
  });
}

export function useCreateProduct() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (p: NewProduct) => request<Product>("/products", { method: "POST", body: p, admin: true }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.products }),
  });
}

export function useSetStock() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { id: string; stock: number }) =>
      request<Product>(`/products/${enc(v.id)}/stock`, { method: "PUT", body: { stock: v.stock }, admin: true }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.products }),
  });
}

export function useServiceStatus() {
  return useQuery({
    queryKey: keys.status,
    queryFn: async () => (await request<{ services: ServiceStatus[] }>("/status")).services,
    refetchInterval: 3_000,
  });
}

/**
 * Asks the Order service whether a key is accepted, without changing anything.
 * Reading the event log of an order that does not exist gives 404 to a valid
 * key and 401/403 to an invalid one, so the answer says exactly that.
 */
export async function checkAdminKey(key: string): Promise<"valid" | "invalid" | "unreachable"> {
  const res = await requestRaw("/orders/ord_key_check/events", {
    headers: { Authorization: `Bearer ${key}` },
  }).catch(() => null);
  if (!res) return "unreachable";
  if (res.status === 404) return "valid";
  if (res.status === 401 || res.status === 403) return "invalid";
  if (res.status >= 500) return "unreachable";
  throw toError(res);
}
