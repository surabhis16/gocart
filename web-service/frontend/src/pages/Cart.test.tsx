import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CartPage } from "./Cart";
import { SignedInProvider } from "../lib/identity";

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function stub(checkout: () => Response | Promise<Response>) {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const path = String(url).replace(/^\/api/, "");
      calls.push(`${init?.method ?? "GET"} ${path}`);
      if (path.startsWith("/products")) return json(200, { items: [{ id: "prd_a", name: "Alpha", description: "", price_minor: 50000, currency: "INR", stock: 5 }] });
      if (path.endsWith("/checkout")) return checkout();
      if (path.startsWith("/carts/")) return json(200, { customer_id: "cust_test", items: [{ product_id: "prd_a", name: "Alpha", quantity: 2 }], revision: 3 });
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
