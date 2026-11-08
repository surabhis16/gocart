import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "./api/client";
import { meKey } from "./lib/identity";
import "@fontsource-variable/bricolage-grotesque";
import "@fontsource-variable/instrument-sans";
import "./index.css";
import { App } from "./App";

const queryClient: QueryClient = new QueryClient({
  // A 401 from any call means the session ended (or was never there): the
  // gate then shows the sign-in panel instead of an error on every screen.
  queryCache: new QueryCache({
    onError: (e, q) => {
      if (e instanceof ApiError && e.status === 401 && q.queryKey[0] !== "me") queryClient.setQueryData(meKey, { signed_in: false });
    },
  }),
  defaultOptions: { queries: { staleTime: 5_000, retry: 1, refetchOnWindowFocus: true } },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
