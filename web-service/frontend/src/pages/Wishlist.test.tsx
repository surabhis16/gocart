import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { WishlistPage } from "./Wishlist";
import { SignedInProvider } from "../lib/identity";

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface MockHandlers {
  products?: unknown | (() => Response | Promise<Response>);
  wishlist?: unknown | (() => Response | Promise<Response>);
  cart?: unknown | (() => Response | Promise<Response>);
  onSetCart?: (body: { quantity?: number }) => void;
  onRemoveWishlist?: (productId: string) => Response | Promise<Response>;
}

function stub(handlers: MockHandlers = {}) {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const path = String(url).replace(/^\/api/, "");
      const method = init?.method ?? "GET";
      calls.push(`${method} ${path}`);

      if (path.startsWith("/products")) {
        if (typeof handlers.products === "function") {
          return handlers.products();
        }
        return json(200, {
          items:
            handlers.products ?? [
              {
                id: "prd_lamp",
                name: "Desk Lamp",
                description: "Warm brass lamp",
                price_minor: 450000,
                currency: "INR",
                stock: 10,
              },
            ],
        });
      }

      if (path.startsWith("/wishlists/cust_test/items/")) {
        const prodId = path.split("/").pop()!;
        if (method === "DELETE") {
          if (handlers.onRemoveWishlist) {
            return handlers.onRemoveWishlist(prodId);
          }
          return json(200, { customer_id: "cust_test", items: [] });
        }
        if (method === "PUT") {
          return json(200, {
            customer_id: "cust_test",
            items: [{ product_id: prodId, quantity: 1, added_at: new Date().toISOString() }],
          });
        }
      }

      if (path === "/wishlists/cust_test") {
        if (method === "DELETE") {
          return new Response(null, { status: 204 });
        }
        if (typeof handlers.wishlist === "function") {
          return handlers.wishlist();
        }
        return json(
          200,
          handlers.wishlist ?? {
            customer_id: "cust_test",
            items: [{ product_id: "prd_lamp", quantity: 2, added_at: new Date().toISOString() }],
          },
        );
      }

      if (path.startsWith("/carts/cust_test/items/")) {
        if (method === "PUT") {
          const body = init?.body ? JSON.parse(String(init.body)) : {};
          handlers.onSetCart?.(body);
          return json(200, {
            customer_id: "cust_test",
            items: [{ product_id: "prd_lamp", name: "Desk Lamp", quantity: body.quantity ?? 1 }],
            revision: 1,
          });
        }
      }

      if (path === "/carts/cust_test") {
        if (typeof handlers.cart === "function") {
          return handlers.cart();
        }
        return json(
          200,
          handlers.cart ?? {
            customer_id: "cust_test",
            items: [{ product_id: "prd_lamp", name: "Desk Lamp", quantity: 1 }],
            revision: 1,
          },
        );
      }

      return json(404, {});
    }),
  );
  return calls;
}

function open() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <SignedInProvider
        value={{
          signed_in: true,
          customer_id: "cust_test",
          name: "Test User",
          email: "test@gocart.test",
          role: "customer",
        }}
      >
        <MemoryRouter initialEntries={["/wishlist"]}>
          <Routes>
            <Route path="/wishlist" element={<WishlistPage />} />
            <Route path="/" element={<p>Shop Page</p>} />
            <Route path="/cart" element={<p>Cart Page</p>} />
          </Routes>
        </MemoryRouter>
      </SignedInProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("WishlistPage", () => {
  it("renders empty state when wishlist is empty", async () => {
    stub({ wishlist: { customer_id: "cust_test", items: [] } });
    open();
    expect(await screen.findByText("Your wishlist is empty")).toBeInTheDocument();
    expect(screen.getByText("Browse the shop")).toBeInTheDocument();
  });

  it("shows catalogue loading state and does not show products as unavailable while useProducts() is loading", async () => {
    let resolveProducts: (r: Response) => void = () => {};
    stub({
      products: () =>
        new Promise<Response>((resolve) => {
          resolveProducts = resolve;
        }),
    });
    open();
    expect(await screen.findByText(/Loading the catalogue/i)).toBeInTheDocument();
    expect(screen.queryByText("Unavailable product")).not.toBeInTheDocument();

    resolveProducts(
      json(200, {
        items: [
          {
            id: "prd_lamp",
            name: "Desk Lamp",
            description: "Warm brass lamp",
            price_minor: 450000,
            currency: "INR",
            stock: 10,
          },
        ],
      }),
    );

    expect(await screen.findByText("Desk Lamp")).toBeInTheDocument();
    expect(screen.queryByText("Unavailable product")).not.toBeInTheDocument();
  });

  it("shows error note if catalogue fails to load and does not mark products as unavailable", async () => {
    stub({
      products: () =>
        json(503, { type: "urn:gocart:problem:service-unavailable", title: "Product service down" }),
    });
    open();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getAllByText(/Product service down/i).length).toBeGreaterThan(0);
    expect(screen.queryByText("Unavailable product")).not.toBeInTheDocument();
  });

  it("disables Add to Cart and Move to Cart buttons while cart data is loading", async () => {
    stub({
      cart: () => new Promise<Response>(() => {}), // Cart never finishes loading
    });
    open();
    expect(await screen.findByText("Desk Lamp")).toBeInTheDocument();
    const addBtn = screen.getByRole("button", { name: "Add Desk Lamp to cart" });
    const moveBtn = screen.getByRole("button", { name: "Move Desk Lamp to cart" });

    expect(addBtn).toBeDisabled();
    expect(moveBtn).toBeDisabled();
    expect(addBtn).toHaveTextContent("Loading cart…");
    expect(moveBtn).toHaveTextContent("Loading cart…");
  });

  it("disables Add to Cart and Move to Cart buttons and shows error note if cart fails to load", async () => {
    stub({
      cart: () => json(500, { type: "urn:gocart:problem:internal", title: "Cart service error" }),
    });
    open();
    expect(await screen.findByText("Desk Lamp")).toBeInTheDocument();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getAllByText(/Cart service error/i).length).toBeGreaterThan(0);

    const addBtn = screen.getByRole("button", { name: "Add Desk Lamp to cart" });
    const moveBtn = screen.getByRole("button", { name: "Move Desk Lamp to cart" });

    expect(addBtn).toBeDisabled();
    expect(moveBtn).toBeDisabled();
    expect(addBtn).toHaveTextContent("Cart unavailable");
    expect(moveBtn).toHaveTextContent("Cart unavailable");
  });

  it("displays saved products with Product Service data", async () => {
    stub();
    open();
    expect(await screen.findByText("Desk Lamp")).toBeInTheDocument();
    expect(screen.getByText("Warm brass lamp")).toBeInTheDocument();
    expect(screen.getByText("10 in stock")).toBeInTheDocument();
    expect(screen.getByText("Saved quantity:")).toBeInTheDocument();
    expect(screen.getByText("2")).toBeInTheDocument();
    expect(screen.getByText(/1 in cart/i)).toBeInTheDocument();
  });

  it("displays unavailable product state if product is missing from catalogue", async () => {
    stub({
      products: [], // Product service catalogue loaded but has no such product
      wishlist: {
        customer_id: "cust_test",
        items: [{ product_id: "prd_ghost", quantity: 1, added_at: new Date().toISOString() }],
      },
    });
    open();
    expect(await screen.findByText("Unavailable product")).toBeInTheDocument();
    expect(screen.getByText("prd_ghost")).toBeInTheDocument();
    expect(screen.getByText(/no longer available in the catalogue/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Remove/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Add to cart/i })).not.toBeInTheDocument();
  });

  it("adds to cart combining existing cart quantity and saved quantity", async () => {
    let setCartBody: { quantity?: number } | null = null;
    stub({
      cart: { customer_id: "cust_test", items: [{ product_id: "prd_lamp", quantity: 3 }], revision: 1 },
      onSetCart: (body) => {
        setCartBody = body;
      },
    });
    open();
    expect(await screen.findByText("Desk Lamp")).toBeInTheDocument();
    const addBtn = screen.getByRole("button", { name: "Add Desk Lamp to cart" });
    await userEvent.click(addBtn);

    // Existing in cart (3) + saved in wishlist (2) = 5
    await waitFor(() => expect(setCartBody).toEqual({ quantity: 5 }));
    expect(await screen.findByText(/total in cart: 5/i)).toBeInTheDocument();
  });

  it("moves to cart: writes to cart first, then removes from wishlist on success", async () => {
    let setCartBody: { quantity?: number } | null = null;
    let removeCalled = false;
    const calls = stub({
      cart: { customer_id: "cust_test", items: [], revision: 1 },
      onSetCart: (body) => {
        setCartBody = body;
      },
      onRemoveWishlist: () => {
        removeCalled = true;
        return json(200, { customer_id: "cust_test", items: [] });
      },
    });
    open();
    expect(await screen.findByText("Desk Lamp")).toBeInTheDocument();
    const moveBtn = screen.getByRole("button", { name: "Move Desk Lamp to cart" });
    await userEvent.click(moveBtn);

    await waitFor(() => expect(setCartBody).toEqual({ quantity: 2 }));
    await waitFor(() => expect(removeCalled).toBe(true));

    // Cart PUT was called before Wishlist DELETE
    const cartIndex = calls.findIndex((c) => c.startsWith("PUT /carts/"));
    const wishlistIndex = calls.findIndex((c) => c.startsWith("DELETE /wishlists/"));
    expect(cartIndex).toBeGreaterThanOrEqual(0);
    expect(wishlistIndex).toBeGreaterThanOrEqual(0);
    expect(cartIndex).toBeLessThan(wishlistIndex);
  });

  it("handles partial success: cart succeeds but wishlist removal fails", async () => {
    stub({
      cart: { customer_id: "cust_test", items: [], revision: 1 },
      onRemoveWishlist: () => json(500, { message: "Failed to remove" }),
    });
    open();
    expect(await screen.findByText("Desk Lamp")).toBeInTheDocument();
    const moveBtn = screen.getByRole("button", { name: "Move Desk Lamp to cart" });
    await userEvent.click(moveBtn);

    expect(await screen.findByText("Partially completed")).toBeInTheDocument();
    expect(screen.getByText(/Both copies have been kept/i)).toBeInTheDocument();
  });

  it("clears the wishlist when Clear button is clicked", async () => {
    const calls = stub();
    open();
    expect(await screen.findByText("Desk Lamp")).toBeInTheDocument();
    const clearBtn = screen.getByRole("button", { name: "Clear wishlist" });
    await userEvent.click(clearBtn);

    await waitFor(() => {
      expect(calls.some((c) => c === "DELETE /wishlists/cust_test")).toBe(true);
    });
  });
});
