import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

const COORDINATOR_PROXY = process.env.WORKBUDDY_COORDINATOR_URL || 'http://127.0.0.1:8090';

const proxiedPrefixes = [
  '/api',
  '/health',
  '/metrics',
  '/events',
  '/tasks',
  '/issues',
  '/sessions',
  '/workers',
  '/login',
  '/logout',
];

const proxy: Record<string, { target: string; changeOrigin: boolean; ws: boolean }> = {};
for (const prefix of proxiedPrefixes) {
  proxy[prefix] = { target: COORDINATOR_PROXY, changeOrigin: true, ws: true };
}

export default defineConfig({
  plugins: [tailwindcss()],
  server: {
    port: 5173,
    strictPort: true,
    proxy,
  },
  build: {
    outDir: 'dist',
    assetsDir: 'assets',
    emptyOutDir: true,
    sourcemap: false,
  },
  esbuild: {
    jsx: 'automatic',
    jsxImportSource: 'preact',
  },
  resolve: {
    alias: {
      react: 'preact/compat',
      'react-dom': 'preact/compat',
    },
  },
});
