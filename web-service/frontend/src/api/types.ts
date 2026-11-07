// Types come from the services' OpenAPI documents (npm run gen:api), so a
// contract change breaks the build here instead of breaking the page later.
import type { components as Product_ } from "./generated/product";
import type { components as Order_ } from "./generated/order";
import type { components as Cart_ } from "./generated/cart";
import type { components as Payment_ } from "./generated/payment";

export type Product = Product_["schemas"]["Product"];
export type NewProduct = Product_["schemas"]["NewProduct"];
export type Order = Order_["schemas"]["Order"];
export type OrderItem = Order_["schemas"]["OrderItem"];
export type OrderStatus = Order_["schemas"]["Status"];
export type PlaceOrderRequest = Order_["schemas"]["PlaceOrderRequest"];
export type Cart = Cart_["schemas"]["Cart"];
export type CartLine = Cart_["schemas"]["Line"];
export type Problem = Order_["schemas"]["Problem"] & Record<string, unknown>;
export type Charge = Payment_["schemas"]["Charge"];

type GeneratedEvent = Order_["schemas"]["Event"];
/** An entry in an order's event log. `data` is whatever the event carries. */
export type OrderEvent = Omit<GeneratedEvent, "data"> & { data: Record<string, unknown> };

export interface ServiceStatus {
  name: string;
  up: boolean;
  latency_ms: number;
}
