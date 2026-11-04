import { defineConfig } from 'vite'

import { foldkit } from '@foldkit/vite-plugin'

export default defineConfig({
  plugins: [foldkit()],
  server: {
    port: 5173,
    // The SPA is served from the same origin as the API in production. In dev
    // the API runs on the Go server, so proxy /api instead of enabling CORS:
    // that keeps cookies first-party in both environments.
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
  },
  // `vite preview` serves the built bundle; the proxy keeps `/api` working so a
  // production-shaped build can be verified before deploying.
  preview: {
    port: 4173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false,
      },
    },
  },
})
