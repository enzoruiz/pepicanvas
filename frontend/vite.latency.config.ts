import { resolve } from "node:path";

import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The TE-010 probe page builds on its own, so it never ships in the
// production build. latencyprobe serves this output with -static.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: resolve(import.meta.dirname, ".cache/latency/dist"),
    emptyOutDir: true,
    rolldownOptions: {
      input: {
        latency: resolve(import.meta.dirname, "latency/index.html"),
      },
    },
  },
});
