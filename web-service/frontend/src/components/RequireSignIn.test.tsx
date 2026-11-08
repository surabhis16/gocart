import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RequireSignIn } from "./RequireSignIn";
import { signInUrl, useCustomerId } from "../lib/identity";

function Secret() {
  return <p>cart of {useCustomerId()}</p>;
}

function open(me: unknown, admin = false, at = "/cart?x=1") {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(me), { status: 200, headers: { "Content-Type": "application/json" } })));
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[at]}>
        <RequireSignIn admin={admin}>
          <Secret />
        </RequireSignIn>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("RequireSignIn", () => {
  it("shows the page to a signed-in customer, with their customer id", async () => {
    open({ signed_in: true, customer_id: "cust_abc", name: "Asha", email: "user1@gocart.test", role: "customer" });
    expect(await screen.findByText("cart of cust_abc")).toBeInTheDocument();
  });

  it("asks everyone else to sign in, and remembers where they were going", async () => {
    open({ signed_in: false });
    const link = await screen.findByRole("link", { name: "Sign in" });
    expect(link).toHaveAttribute("href", signInUrl("/cart?x=1"));
    expect(screen.queryByText(/cart of/)).not.toBeInTheDocument();
  });

  it("keeps the admin area for administrators", async () => {
    open({ signed_in: true, customer_id: "cust_abc", name: "Asha", email: "user1@gocart.test", role: "customer" }, true, "/admin");
    expect(await screen.findByText(/not an administrator account/)).toBeInTheDocument();
    expect(screen.queryByText(/cart of/)).not.toBeInTheDocument();
  });

  it("lets an administrator in", async () => {
    open({ signed_in: true, customer_id: "cust_boss", name: "Admin", email: "admin@gocart.test", role: "admin" }, true, "/admin");
    expect(await screen.findByText("cart of cust_boss")).toBeInTheDocument();
  });
});

describe("signInUrl", () => {
  it("encodes where to go back to", () => {
    expect(signInUrl("/orders/ord_1?a=b&c=d")).toBe("/auth/login?next=%2Forders%2Ford_1%3Fa%3Db%26c%3Dd");
  });
});
