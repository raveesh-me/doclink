import { createRouter, createWebHistory } from "vue-router";
import ItemList from "./views/ItemList.vue";
import ItemDetail from "./views/ItemDetail.vue";

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: "/", redirect: "/items" },
    { path: "/items", name: "items", component: ItemList },
    { path: "/items/:id", name: "item", component: ItemDetail, props: true },
  ],
});
