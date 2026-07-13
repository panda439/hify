import path from 'node:path'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    proxy: {
      // Go backend during `npm run dev`; prod build is embedded via go:embed
      // and served from the same origin, so this proxy is dev-only.
      '/api': 'http://127.0.0.1:8080',
    },
  },
})
