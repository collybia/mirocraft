import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The production build lands in web/dist, which the Go binary embeds.
// In development the API is proxied so the panel and the daemon share an
// origin, which keeps the WebSocket upgrade and its same-origin check honest.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: "http://127.0.0.1:8080",
        changeOrigin: true,
        ws: true,
      },
    },
  },
});
