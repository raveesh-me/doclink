<script setup lang="ts">
import { ref, onMounted, watch } from "vue";
import { useRouter } from "vue-router";
import { itemClient, formatPrice } from "../api.js";
import type { Item } from "@doclink/gen/pim/v1/item_pb.js";

const router = useRouter();
const items = ref<Item[]>([]);
const total = ref(0);
const query = ref("");
const loading = ref(true);
const error = ref("");

async function load() {
  loading.value = true;
  error.value = "";
  try {
    const res = await itemClient.listItems({ pageSize: 50, query: query.value });
    items.value = res.items;
    total.value = res.total;
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e);
  } finally {
    loading.value = false;
  }
}

let debounce: number | undefined;
watch(query, () => {
  window.clearTimeout(debounce);
  debounce = window.setTimeout(load, 250);
});

onMounted(load);
</script>

<template>
  <div>
    <div style="display: flex; align-items: center; gap: 12px; margin-bottom: 16px">
      <input v-model="query" placeholder="Search items by title or SKU" style="flex: 1" />
      <span class="muted">{{ total }} items</span>
    </div>

    <p v-if="error" class="muted">Could not load items: {{ error }}</p>

    <div class="card" style="overflow: hidden">
      <table>
        <thead>
          <tr>
            <th style="width: 130px">SKU</th>
            <th>Title</th>
            <th style="width: 110px">Price</th>
            <th style="width: 90px">Status</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="item in items" :key="item.id" @click="router.push(`/items/${item.id}`)">
            <td class="mono muted">{{ item.sku }}</td>
            <td>{{ item.title }}</td>
            <td>{{ formatPrice(item.priceCents, item.currency) }}</td>
            <td class="muted">{{ item.status }}</td>
          </tr>
          <tr v-if="!loading && items.length === 0">
            <td colspan="4" class="muted" style="text-align: center; padding: 32px">
              No items match “{{ query }}”.
            </td>
          </tr>
          <tr v-if="loading">
            <td colspan="4" class="muted" style="text-align: center; padding: 32px">Loading…</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
