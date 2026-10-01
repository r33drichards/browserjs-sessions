import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

const backend = "http://localhost:8080"

export default defineConfig({
  plugins: [react()],
  optimizeDeps: { include: ["@cloudscape-design/components"] },
  server: {
    proxy: {
      "/api": backend,
      "/config.js": backend,
      "/s": { target: backend, ws: true },
    },
  },
})
