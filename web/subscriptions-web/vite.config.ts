import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import { resolve } from "node:path";

export default defineConfig({
  plugins: [vue()],
  server: { port: 5182, strictPort: true, cors: true },
  build: {
    target: "es2022",
    rollupOptions: {
      // The card is the only page this app serves. There is no "subscriptions
      // app" shell in this POC: the satellite's entire UI contribution is the
      // embedded card.
      input: { itemCard: resolve(__dirname, "embed/item-card.html") },
    },
  },
});
