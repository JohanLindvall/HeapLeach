import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The build lands directly in the Go package that embeds it, so `go build`
// always picks up the latest UI without a copy step.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../backend/internal/webui/dist',
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 700,
  },
  server: {
    port: 5173,
    strictPort: true,
    // `make dev` runs the Go server on :8080; proxying keeps the browser on
    // one origin so EventSource needs no CORS handling.
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
});
