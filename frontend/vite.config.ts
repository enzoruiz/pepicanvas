import { resolve } from "node:path";

import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  server: {
    // The API runs in the Go server; proxying keeps one origin, so the
    // session cookie and the Origin check behave as in production.
    proxy: {
      "/api": process.env.PEPICANVAS_API_TARGET ?? "http://127.0.0.1:8080",
    },
  },
  build: {
    rolldownOptions: {
      input: {
        editor: resolve(import.meta.dirname, "editor/index.html"),
        overlay: resolve(import.meta.dirname, "overlay/index.html"),
      },
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
  },
});
