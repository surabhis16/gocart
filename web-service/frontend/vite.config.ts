/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` serves the app on :5173 and forwards /api to the Go gateway
// (web-service), which must be running with the four services behind it:
//   docker compose up           # the whole stack, gateway on :3000
//   GATEWAY=http://localhost:3000 npm run dev
const gateway = process.env.GATEWAY ?? "http://localhost:3000";

export default defineConfig({
  plugins: [react()],
  // The Go gateway embeds this folder (see internal/ui), so the build goes there.
  build: { outDir: "../internal/ui/dist", emptyOutDir: false, sourcemap: false },
  server: { port: 5173, proxy: { "/api": gateway } },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: false,
  },
});
