import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// DUBARR_WEB_OUT is a BUILD-TIME-only knob for where Vite emits static
// assets — it is NOT app runtime configuration, and the zero-env policy for
// the running appliance still holds (dubarr reads only --data-dir/--listen/
// --desktop). Default outDir is 'dist'; T7 wires the output to backend/web
// via the build script so the Go binary can embed it.
const outDir = process.env.DUBARR_WEB_OUT ?? 'dist'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir,
    emptyOutDir: true,
  },
})
