import { defineConfig } from "vite";
import { resolve } from "node:path";

// No framework plugin, and no framework dependency in package.json. This app
// exists to prove the host contract does not impose a stack: the shell is Vue,
// this card is hand-written TypeScript against the DOM, and the host cannot
// tell the difference.
export default defineConfig({
  server: { port: 5183, strictPort: true, cors: true },
  build: {
    target: "es2022",
    rollupOptions: {
      input: { itemCard: resolve(__dirname, "embed/item-card.html") },
    },
  },
});
