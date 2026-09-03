import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Every gateway path the browser may reach, proxied in dev and in preview so
// the console is same-origin in both. The gateway has no CORS middleware and
// is not getting one; see the phase plan's Global Constraints.
const gateway = "http://localhost:8080";
const proxy = {
  "/api": gateway,
  "/health": gateway,
  "/ready": gateway,
  "/metrics": gateway,
};

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: { proxy },
  preview: { proxy },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: false,
    restoreMocks: true,
  },
});
