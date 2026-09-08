import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5181,
    strictPort: true,
    // The shell and the embeds are deliberately different origins. In kind they
    // get distinct hostnames; locally distinct ports is the closest equivalent
    // that still exercises the cross-origin path in the SDK.
    cors: true,
  },
  // Backend endpoints are read at runtime from window.__DOCLINK_CONFIG__ (see
  // index.html) rather than baked in at build time, so the same image can be
  // deployed to kind and to a laptop without rebuilding.
  build: { target: "es2022" },
});
