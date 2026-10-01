import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

const backend = "http://localhost:8080"

export default defineConfig({
  plugins: [react()],
  optimizeDeps: { include: ["@cloudscape-design/components"] },
  // noVNC uses top-level await, which Vite's default build target rejects.
  build: { target: "es2022" },
  server: {
    proxy: {
      "/api": backend,
      "/config.js": backend,
      "/s": { target: backend, ws: true },
    },
  },
})
