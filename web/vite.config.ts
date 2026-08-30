import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Paths are relative to this file's directory, which keeps the config free of
// Node built-ins and therefore free of @types/node.
//
// The build lands directly in the Go package that embeds it, so `go build`
// produces one binary with the front-end inside. Filenames are content-hashed
// and resolved through the manifest, which lets the server cache them forever.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/httpserver/web/static",
    emptyOutDir: true,
    manifest: "manifest.json",
    rollupOptions: {
      input: {
        queue: "src/queue/main.ts",
        admin: "src/admin/main.tsx",
      },
    },
    target: "es2020",
  },
  server: {
    // `npm run dev` serves the assets while anteroom itself runs on 8080.
    proxy: {
      "/__anteroom": "http://localhost:8080",
    },
  },
});
