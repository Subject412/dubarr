import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Production assets are emitted straight into backend/web so the Go binary can
// embed the real SPA (`//go:embed *` in backend/web/doc.go). emptyOutDir MUST
// stay false: emptying that directory would delete doc.go and break the embed.
// DUBARR_WEB_OUT is a build-time-only override for local experiments.
const outDir = process.env.DUBARR_WEB_OUT ?? '../backend/web'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir,
    emptyOutDir: false,
  },
})
