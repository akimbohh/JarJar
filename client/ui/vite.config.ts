import { defineConfig } from "vite";

// Tauri serves the UI as static assets; no framework, single entry.
// The dev server port is fixed so tauri.conf.json can point devUrl at it.
export default defineConfig({
  // Relative base so the compiled assets load from tauri's asset protocol.
  base: "./",
  clearScreen: false,
  server: {
    port: 5173,
    strictPort: true,
  },
  build: {
    // Match tauri.conf.json build.frontendDist (../ui/dist).
    outDir: "dist",
    emptyOutDir: true,
    target: "es2021",
    sourcemap: false,
  },
});
