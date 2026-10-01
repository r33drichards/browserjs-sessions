import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

const backend = "http://localhost:8080"

export default defineConfig({
  plugins: [react()],
  // noVNC uses top-level await, which Vite's default target rejects, both in
  // the production build and in the dev server's dependency pre-bundling.
  optimizeDeps: { include: ["@cloudscape-design/components"], esbuildOptions: { target: "es2022" } },
  build: { target: "es2022" },
  server: {
    proxy: {
      "/api": backend,
      "/config.js": backend,
      // Trailing slash matters: keys are prefix matches, and "/s" would also
      // swallow /src/… (Vite's own modules) and the /sessions/… routes.
      "/s/": { target: backend, ws: true },
    },
  },
})
