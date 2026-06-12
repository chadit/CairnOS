/// <reference types="vitest/config" />
import { fileURLToPath, URL } from 'node:url';

import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig, type Plugin } from 'vite';

// The gateway base URL the dev proxy forwards to. Override with
// CAIRNOS_GATEWAY when the HTTP gateway listens somewhere other than the
// default `cairnos serve` bind.
const gateway = process.env.CAIRNOS_GATEWAY ?? 'http://localhost:8080';

// productionCSP injects a strict Content-Security-Policy meta tag into the
// built index.html, the M2 hardening that keeps the localStorage bearer token
// from being exfiltrated by injected script (.agents/spec/14-open-decisions.md,
// M2). It is build-only on purpose: the dev server needs inline scripts, eval,
// and a websocket for HMR, which this policy would block.
//
// The built SPA loads only its own hashed bundle and talks to a same-origin
// API, so 'self' covers script and connect. style keeps 'unsafe-inline'
// because React and reactflow set element styles inline. A cross-origin API
// deployment must widen connect-src to name the API origin, and frame-ancestors
// can only come from a real response header, so the static host should also
// send `Content-Security-Policy: frame-ancestors 'none'` (see web/README.md).
function productionCSP(): Plugin {
  const policy = [
    "default-src 'self'",
    "script-src 'self'",
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    "font-src 'self'",
    "connect-src 'self'",
    "object-src 'none'",
    "base-uri 'self'",
  ].join('; ');

  return {
    name: 'cairnos-production-csp',
    apply: 'build',
    transformIndexHtml() {
      return [
        {
          tag: 'meta',
          attrs: { 'http-equiv': 'Content-Security-Policy', content: policy },
          injectTo: 'head-prepend',
        },
      ];
    },
  };
}

export default defineConfig({
  plugins: [react(), tailwindcss(), productionCSP()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5173,
    proxy: {
      // Every API call goes through /v1. Proxying it in dev means the SPA
      // and the gateway share an origin, so no CORS config is needed and the
      // streaming /v1/workflows/.../runs response passes through untouched.
      '/v1': {
        target: gateway,
        changeOrigin: true,
      },
    },
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./vitest.setup.ts'],
    css: false,
    coverage: {
      provider: 'v8',
      reporter: ['text', 'html'],
      include: ['src/**/*.{ts,tsx}'],
      exclude: ['src/**/*.test.{ts,tsx}', 'src/test/**', 'src/main.tsx'],
    },
  },
});
