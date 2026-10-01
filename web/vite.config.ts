import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// The backend takes the user's identity from a header set by the identity-aware
// proxy in front of it. In dev, point this at such a proxy (one that injects the
// identity header), or run the backend with its dev settings.
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
    },
  },
})
