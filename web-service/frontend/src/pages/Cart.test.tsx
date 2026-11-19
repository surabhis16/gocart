import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CartPage } from "./Cart";
import { SignedInProvider } from "../lib/identity";

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface StubOptions {
  checkout?: () => Response | Promise<Response>;
  wishlist?: unknown | (() => Response | Promise<Response>);
  cart?: unknown;
  onSetWishlist?: (body: { quantity?: number }) => Response | Promise<Response>;
  onRemoveCart?: (productId: string) => Response | Promise<Response>;
  onSetCart?: (body: { quantity?: number }) => void;
}

function stub(optsOrCheckout?: StubOptions | (() => Response | Promise<Response>)) {
  const opts: StubOptions =
    typeof optsOrCheckout === "function" ? { checkout: optsOrCheckout } : optsOrCheckout ?? {};
  const calls: string[] = [];

  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const path = String(url).replace(/^\/api/, "");
      const method = init?.method ?? "GET";
      calls.push(`${method} ${path}`);

      if (path.startsWith("/products")) {
        return json(200, {
          items: [{ id: "prd_a", name: "Alpha", description: "", price_minor: 50000, currency: "INR", stock: 5 }],
        });
      }

      if (path.endsWith("/checkout")) {
        if (opts.checkout) return opts.checkout();
        return json(201, { id: "ord_1", status: "confirmed", items: [], customer_id: "cust_test" });
      }

      if (path.startsWith("/wishlists/cust_test/items/")) {
        if (method === "PUT") {
          const body = init?.body ? JSON.parse(String(init.body)) : {};
          if (opts.onSetWishlist) return opts.onSetWishlist(body);
          return json(200, {
            customer_id: "cust_test",
            items: [{ product_id: "prd_a", quantity: body.quantity ?? 1 }],
          });
        }
      }

      if (path === "/wishlists/cust_test") {
        if (typeof opts.wishlist === "function") return opts.wishlist();
        return json(
          200,
          opts.wishlist ?? {
            customer_id: "cust_test",
            items: [],
          },
        );
      }

      if (path.startsWith("/carts/cust_test/items/")) {
        if (method === "DELETE") {
          const prodId = path.split("/").pop()!;
          if (opts.onRemoveCart) return opts.onRemoveCart(prodId);
          return json(200, { customer_id: "cust_test", items: [], revision: 4 });
        }
        if (method === "PUT") {
          const body = init?.body ? JSON.parse(String(init.body)) : {};
          opts.onSetCart?.(body);
          return json(200, {
            customer_id: "cust_test",
            items: [{ product_id: "prd_a", name: "Alpha", quantity: body.quantity ?? 1 }],
            revision: 4,
          });
        }
      }

      if (path.startsWith("/carts/")) {
        return json(
          200,
          opts.cart ?? {
            customer_id: "cust_test",
            items: [{ product_id: "prd_a", name: "Alpha", quantity: 2 }],
            revision: 3,
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
      <SignedInProvider value={{ signed_in: true, customer_id: "cust_test", name: "Test", email: "t@gocart.test", role: "customer" }}>
        <MemoryRouter initialEntries={["/cart"]}>
          <Routes>
            <Route path="/cart" element={<CartPage />} />
            <Route path="/orders/:id" element={<p>receipt page</p>} />
          </Routes>
        </MemoryRouter>
      </SignedInProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("CartPage checkout", () => {
  it("shows the estimate and goes to the receipt on success", async () => {
    stub(() => json(201, { id: "ord_1", status: "confirmed", items: [], customer_id: "cust_test" }));
    open();
    expect(await screen.findByText(/Estimated total/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^Pay/ }));
    expect(await screen.findByText("receipt page")).toBeInTheDocument();
  });

  it("keeps the cart and says nothing was charged when the card is declined", async () => {
    stub(() => json(402, { type: "urn:gocart:problem:payment-declined", decline_code: "insufficient_funds" }));
    open();
    await userEvent.click(await screen.findByRole("button", { name: /^Pay/ }));
    expect(await screen.findByText(/insufficient funds/i)).toBeInTheDocument();
    expect(screen.getByText(/You were not charged/)).toBeInTheDocument();
    expect(screen.getByText("Alpha")).toBeInTheDocument();
  });

  it("does not claim failure when the store gives no answer, and offers a safe retry", async () => {
    stub(() => json(503, { type: "urn:gocart:problem:upstream-unavailable" }));
    open();
    await userEvent.click(await screen.findByRole("button", { name: /^Pay/ }));
    expect(await screen.findByText(/could not confirm/i)).toBeInTheDocument();
    expect(screen.getByText(/cannot order twice/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Check and pay again/ })).toBeInTheDocument();
  });

  it("sends one checkout per click even when the button is pressed twice quickly", async () => {
    let release: (r: Response) => void = () => {};
    const calls = stub(() => new Promise<Response>((r) => (release = r)));
    open();
    const pay = await screen.findByRole("button", { name: /^Pay/ });
    await userEvent.dblClick(pay);
    release(json(201, { id: "ord_1", status: "confirmed", items: [], customer_id: "cust_test" }));
    await waitFor(() => expect(screen.getByText("receipt page")).toBeInTheDocument());
    expect(calls.filter((c) => c.endsWith("/checkout"))).toHaveLength(1);
  });
});

describe("CartPage Save for later", () => {
  it("saves a cart item to an empty wishlist using destination-first flow", async () => {
    let savedBody: { quantity?: number } | null = null;
    let cartRemoved = false;
    const calls = stub({
      wishlist: { customer_id: "cust_test", items: [] },
      onSetWishlist: (body) => {
        savedBody = body;
        return json(200, {
          customer_id: "cust_test",
          items: [{ product_id: "prd_a", quantity: body.quantity ?? 1 }],
        });
      },
      onRemoveCart: () => {
        cartRemoved = true;
        return json(200, { customer_id: "cust_test", items: [], revision: 4 });
      },
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const saveBtn = screen.getByRole("button", { name: /Save Alpha for later/i });
    await userEvent.click(saveBtn);

    await waitFor(() => expect(savedBody).toEqual({ quantity: 2 }));
    await waitFor(() => expect(cartRemoved).toBe(true));

    const wishIndex = calls.findIndex((c) => c.startsWith("PUT /wishlists/"));
    const cartIndex = calls.findIndex((c) => c.startsWith("DELETE /carts/"));
    expect(wishIndex).toBeGreaterThanOrEqual(0);
    expect(cartIndex).toBeGreaterThanOrEqual(0);
    expect(wishIndex).toBeLessThan(cartIndex);

    expect(await screen.findByText(/Saved Alpha to your wishlist/i)).toBeInTheDocument();
  });

  it("combines existing wishlist quantity and cart quantity when saving", async () => {
    let savedBody: { quantity?: number } | null = null;
    stub({
      wishlist: {
        customer_id: "cust_test",
        items: [{ product_id: "prd_a", quantity: 3, added_at: new Date().toISOString() }],
      },
      onSetWishlist: (body) => {
        savedBody = body;
        return json(200, {
          customer_id: "cust_test",
          items: [{ product_id: "prd_a", quantity: body.quantity ?? 1 }],
        });
      },
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const saveBtn = screen.getByRole("button", { name: /Save Alpha for later/i });
    await userEvent.click(saveBtn);

    // Existing wishlist (3) + cart quantity (2) = 5
    await waitFor(() => expect(savedBody).toEqual({ quantity: 5 }));
  });

  it("leaves the cart item untouched if wishlist save fails", async () => {
    const calls = stub({
      onSetWishlist: () =>
        json(500, { type: "urn:gocart:problem:internal", title: "Wishlist write error" }),
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const saveBtn = screen.getByRole("button", { name: /Save Alpha for later/i });
    await userEvent.click(saveBtn);

    expect(await screen.findByText(/Could not save for later/i)).toBeInTheDocument();
    expect(screen.getByText(/The item remains in your cart/i)).toBeInTheDocument();
    expect(screen.getByText("Alpha")).toBeInTheDocument();

    // Ensure DELETE was NEVER called on Cart
    expect(calls.some((c) => c.startsWith("DELETE /carts/"))).toBe(false);
  });

  it("retains both copies and displays a warning if cart removal fails after wishlist succeeds", async () => {
    stub({
      onRemoveCart: () =>
        json(500, { type: "urn:gocart:problem:internal", title: "Cart delete error" }),
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const saveBtn = screen.getByRole("button", { name: /Save Alpha for later/i });
    await userEvent.click(saveBtn);

    expect(await screen.findByText(/Partially saved/i)).toBeInTheDocument();
    expect(screen.getByText(/Both copies have been kept/i)).toBeInTheDocument();
    expect(screen.getByText("Alpha")).toBeInTheDocument();
  });

  it("refuses to save and shows a warning when combined quantity exceeds 100", async () => {
    const calls = stub({
      cart: {
        customer_id: "cust_test",
        items: [{ product_id: "prd_a", name: "Alpha", quantity: 60 }],
        revision: 3,
      },
      wishlist: {
        customer_id: "cust_test",
        items: [{ product_id: "prd_a", quantity: 50, added_at: new Date().toISOString() }],
      },
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const saveBtn = screen.getByRole("button", { name: /Save Alpha for later/i });
    await userEvent.click(saveBtn);

    // Combined = 110 > 100
    expect(await screen.findByText(/Cannot save for later/i)).toBeInTheDocument();
    expect(screen.getByText(/exceeding the maximum allowed of 100/i)).toBeInTheDocument();
    expect(screen.getByText("Alpha")).toBeInTheDocument();

    // Neither Wishlist PUT nor Cart DELETE was called
    expect(calls.some((c) => c.startsWith("PUT /wishlists/"))).toBe(false);
    expect(calls.some((c) => c.startsWith("DELETE /carts/"))).toBe(false);
  });

  it("preserves quantity increment and decrement in the cart", async () => {
    let updatedQty: number | null = null;
    stub({
      onSetCart: (body) => {
        updatedQty = body.quantity ?? null;
      },
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const plusBtn = screen.getByRole("button", { name: "Add one more" });
    await userEvent.click(plusBtn);

    await waitFor(() => expect(updatedQty).toBe(3));
  });
});

describe("Cart removal confirmation modal", () => {
  it("opens confirmation modal when bin icon is clicked and does not immediately remove item", async () => {
    const calls = stub();
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(screen.getByText("Remove item from cart?")).toBeInTheDocument();
    expect(screen.getByText("What would you like to do with this product?")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove from cart" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Move to Wishlist" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();

    // Ensure item was not immediately removed
    expect(calls.some((c) => c.startsWith("DELETE /carts/"))).toBe(false);
  });

  it("closes modal on Cancel and leaves item in cart", async () => {
    const calls = stub();
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    await userEvent.click(cancelBtn);

    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(screen.getByText("Alpha")).toBeInTheDocument();
    expect(calls.some((c) => c.startsWith("DELETE /carts/"))).toBe(false);
    expect(calls.some((c) => c.startsWith("PUT /wishlists/"))).toBe(false);
  });

  it("closes modal on Escape key and returns focus to bin button", async () => {
    stub();
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");

    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    await waitFor(() => expect(trashBtn).toHaveFocus());
  });

  it("removes item from cart when 'Remove from cart' is clicked and closes modal", async () => {
    let removed = false;
    const calls = stub({
      onRemoveCart: () => {
        removed = true;
        return json(200, { customer_id: "cust_test", items: [], revision: 4 });
      },
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    const removeBtn = await screen.findByRole("button", { name: "Remove from cart" });
    await userEvent.click(removeBtn);

    await waitFor(() => expect(removed).toBe(true));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.some((c) => c.startsWith("PUT /wishlists/"))).toBe(false);
  });

  it("keeps modal open and displays error if 'Remove from cart' fails", async () => {
    stub({
      onRemoveCart: () => json(500, { type: "urn:gocart:problem:internal", title: "Delete failure" }),
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    const removeBtn = await screen.findByRole("button", { name: "Remove from cart" });
    await userEvent.click(removeBtn);

    expect(await screen.findByText(/Could not remove item/i)).toBeInTheDocument();
    expect(screen.getByText(/Failed to remove Alpha from your cart/i)).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("moves item to Wishlist using destination-first flow, combines quantities, and closes modal", async () => {
    let savedBody: { quantity?: number } | null = null;
    let cartRemoved = false;
    const calls = stub({
      wishlist: {
        customer_id: "cust_test",
        items: [{ product_id: "prd_a", quantity: 3, added_at: new Date().toISOString() }],
      },
      onSetWishlist: (body) => {
        savedBody = body;
        return json(200, { customer_id: "cust_test", items: [{ product_id: "prd_a", quantity: body.quantity ?? 1 }] });
      },
      onRemoveCart: () => {
        cartRemoved = true;
        return json(200, { customer_id: "cust_test", items: [], revision: 4 });
      },
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    const moveBtn = await screen.findByRole("button", { name: "Move to Wishlist" });
    await userEvent.click(moveBtn);

    await waitFor(() => expect(savedBody).toEqual({ quantity: 5 }));
    await waitFor(() => expect(cartRemoved).toBe(true));

    const wishIndex = calls.findIndex((c) => c.startsWith("PUT /wishlists/"));
    const cartIndex = calls.findIndex((c) => c.startsWith("DELETE /carts/"));
    expect(wishIndex).toBeGreaterThanOrEqual(0);
    expect(cartIndex).toBeGreaterThanOrEqual(0);
    expect(wishIndex).toBeLessThan(cartIndex);

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await screen.findByText(/Moved Alpha to your wishlist/i)).toBeInTheDocument();
  });

  it("keeps modal open and shows error when Wishlist save fails during Move to Wishlist", async () => {
    const calls = stub({
      onSetWishlist: () => json(500, { type: "urn:gocart:problem:internal", title: "Wishlist fail" }),
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    const moveBtn = await screen.findByRole("button", { name: "Move to Wishlist" });
    await userEvent.click(moveBtn);

    expect(await screen.findByText(/Could not move to wishlist/i)).toBeInTheDocument();
    expect(screen.getByText(/Failed to save Alpha to your wishlist/i)).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    // Cart DELETE was not called
    expect(calls.some((c) => c.startsWith("DELETE /carts/"))).toBe(false);
  });

  it("keeps modal open and warns if Cart deletion fails after Wishlist succeeds during Move to Wishlist", async () => {
    stub({
      onRemoveCart: () => json(500, { type: "urn:gocart:problem:internal", title: "Cart delete fail" }),
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    const moveBtn = await screen.findByRole("button", { name: "Move to Wishlist" });
    await userEvent.click(moveBtn);

    expect(await screen.findByText(/Partially moved/i)).toBeInTheDocument();
    expect(screen.getByText(/Both copies have been kept; please try removing from cart again/i)).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("warns and leaves Cart item untouched if combined quantity exceeds 100", async () => {
    const calls = stub({
      cart: {
        customer_id: "cust_test",
        items: [{ product_id: "prd_a", name: "Alpha", quantity: 60 }],
        revision: 3,
      },
      wishlist: {
        customer_id: "cust_test",
        items: [{ product_id: "prd_a", quantity: 50, added_at: new Date().toISOString() }],
      },
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    const moveBtn = await screen.findByRole("button", { name: "Move to Wishlist" });
    await userEvent.click(moveBtn);

    expect(await screen.findByText(/Cannot move to wishlist/i)).toBeInTheDocument();
    expect(screen.getByText(/exceeding the maximum allowed of 100/i)).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    expect(calls.some((c) => c.startsWith("PUT /wishlists/"))).toBe(false);
    expect(calls.some((c) => c.startsWith("DELETE /carts/"))).toBe(false);
  });

  it("disables buttons and ignores Escape while operation is in progress", async () => {
    let resolveDelete: (r: Response) => void = () => {};
    stub({
      onRemoveCart: () => new Promise<Response>((r) => (resolveDelete = r)),
    });
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    const removeBtn = await screen.findByRole("button", { name: "Remove from cart" });
    const moveBtn = screen.getByRole("button", { name: "Move to Wishlist" });
    const cancelBtn = screen.getByRole("button", { name: "Cancel" });

    await userEvent.click(removeBtn);

    // Now removing is in progress
    await waitFor(() => expect(removeBtn).toBeDisabled());
    expect(moveBtn).toBeDisabled();
    expect(cancelBtn).toBeDisabled();

    // Escape should not close modal while in progress
    await userEvent.keyboard("{Escape}");
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    // Complete operation
    resolveDelete(json(200, { customer_id: "cust_test", items: [], revision: 4 }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("traps keyboard Tab navigation inside the modal", async () => {
    stub();
    open();
    expect(await screen.findByText("Alpha")).toBeInTheDocument();
    const trashBtn = screen.getByRole("button", { name: /Remove Alpha from the cart/i });
    await userEvent.click(trashBtn);

    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    const removeBtn = screen.getByRole("button", { name: "Remove from cart" });
    const moveBtn = screen.getByRole("button", { name: "Move to Wishlist" });
    const cancelBtn = screen.getByRole("button", { name: "Cancel" });

    // Initial focus on first button
    await waitFor(() => expect(removeBtn).toHaveFocus());

    await userEvent.tab();
    expect(moveBtn).toHaveFocus();

    await userEvent.tab();
    expect(cancelBtn).toHaveFocus();

    // Tab from last wraps to first
    await userEvent.tab();
    expect(removeBtn).toHaveFocus();

    // Shift-Tab from first wraps to last
    await userEvent.tab({ shift: true });
    expect(cancelBtn).toHaveFocus();
  });
});
