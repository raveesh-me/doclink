<script setup lang="ts">
/**
 * The item detail screen: the demonstration surface for the whole experiment.
 *
 * Above the fold, fields the PIM team owns. Below it, cards this file has never
 * heard of, discovered from the registry at page load and handed nothing but the
 * item's DocRef.
 */
import { ref, computed, onMounted, watch } from "vue";
import { useRouter } from "vue-router";
import {
  itemClient,
  registryClient,
  ITEM_DETAIL_SLOT,
  itemDocRef,
  formatPrice,
} from "../api.js";
import type { Item } from "@doclink/gen/pim/v1/item_pb.js";
import type { Contribution, GetLinksResponse } from "@doclink/gen/doclink/v1/registry_pb.js";
import { ResolutionStrategy } from "@doclink/gen/doclink/v1/core_pb.js";
import EmbedCard from "../components/EmbedCard.vue";
import { pushToast } from "../toasts.js";

const props = defineProps<{ id: string }>();
const router = useRouter();

const item = ref<Item | null>(null);
const loadError = ref("");
const saving = ref(false);

const available = ref<Contribution[]>([]);
const mounted = ref<string[]>([]);

const links = ref<GetLinksResponse | null>(null);
const strategy = ref<ResolutionStrategy>(ResolutionStrategy.FEDERATED);

const anchorRef = computed(() => itemDocRef(props.id));

const mountedContributions = computed(() =>
  mounted.value
    .map((id) => available.value.find((c) => c.id === id))
    .filter((c): c is Contribution => c !== undefined),
);
const addable = computed(() => available.value.filter((c) => !mounted.value.includes(c.id)));

/** Which cards the user has open is a per-viewer UI preference, so localStorage. */
const storageKey = computed(() => `doclink:cards:${props.id}`);

function loadMountedState(defaults: string[]) {
  try {
    const raw = localStorage.getItem(storageKey.value);
    mounted.value = raw ? (JSON.parse(raw) as string[]) : defaults;
  } catch {
    mounted.value = defaults;
  }
}

function persistMountedState() {
  try {
    localStorage.setItem(storageKey.value, JSON.stringify(mounted.value));
  } catch {
    /* ignore */
  }
}

async function loadItem() {
  loadError.value = "";
  try {
    const res = await itemClient.getItem({ id: props.id });
    item.value = res.item ?? null;
  } catch (e) {
    loadError.value = e instanceof Error ? e.message : String(e);
  }
}

/**
 * Ask the registry which cards exist for this slot.
 *
 * This request is the runtime replacement for an import. Nothing in this bundle
 * names subscriptions or shipping; they appear because they registered.
 */
async function loadContributions() {
  const res = await registryClient.listContributions({ extensionPointId: ITEM_DETAIL_SLOT });
  available.value = res.contributions;
  loadMountedState(res.contributions.map((c) => c.id));
}

/** The read-side summary, used here to make the three strategies visible. */
async function loadLinks() {
  try {
    links.value = await registryClient.getLinks({
      anchor: { namespace: "pim", type: "item", id: props.id },
      strategy: strategy.value,
    });
  } catch (e) {
    pushToast("error", e instanceof Error ? e.message : String(e), "doclink");
  }
}

async function save() {
  if (!item.value) return;
  saving.value = true;
  try {
    const res = await itemClient.updateItem({ item: item.value });
    item.value = res.item ?? item.value;
    pushToast("success", "Item saved", "pim");
  } catch (e) {
    pushToast("error", e instanceof Error ? e.message : String(e), "pim");
  } finally {
    saving.value = false;
  }
}

async function remove() {
  if (!window.confirm("Delete this item? Satellites will reconcile their own links.")) return;
  try {
    await itemClient.deleteItem({ id: props.id });
    pushToast("success", "Item deleted; lifecycle event queued", "pim");
    router.push("/items");
  } catch (e) {
    pushToast("error", e instanceof Error ? e.message : String(e), "pim");
  }
}

function addCard(id: string) {
  if (!id || mounted.value.includes(id)) return;
  mounted.value = [...mounted.value, id];
}

function removeCard(id: string) {
  mounted.value = mounted.value.filter((c) => c !== id);
}

watch(mounted, persistMountedState, { deep: true });
watch(strategy, loadLinks);
watch(
  () => props.id,
  () => {
    void loadItem();
    void loadContributions();
    void loadLinks();
  },
);

onMounted(() => {
  void loadItem();
  void loadContributions();
  void loadLinks();
});

const strategyOptions = [
  { value: ResolutionStrategy.FEDERATED, label: "Federated (fan-out)" },
  { value: ResolutionStrategy.INDEXED, label: "Indexed (registry cache)" },
  { value: ResolutionStrategy.MONOLITH, label: "Monolith (baseline)" },
];
</script>

<template>
  <div>
    <p style="margin: 0 0 16px">
      <RouterLink to="/items" class="muted">← All items</RouterLink>
    </p>

    <p v-if="loadError" class="muted">Could not load item: {{ loadError }}</p>

    <!-- Canonical fields: everything the PIM team owns, and the complete list of
         columns on pim.items. -->
    <section v-if="item" class="card panel">
      <header>
        <strong>Item</strong>
        <span class="mono muted">{{ anchorRef }}</span>
        <span class="spacer" />
        <button class="danger" @click="remove">Delete</button>
        <button class="primary" :disabled="saving" @click="save">
          {{ saving ? "Saving…" : "Save" }}
        </button>
      </header>

      <div class="grid">
        <label>SKU<input v-model="item.sku" /></label>
        <label>Status<input v-model="item.status" /></label>
        <label class="wide">Title<input v-model="item.title" /></label>
        <label class="wide">Description<input v-model="item.description" /></label>
        <label>
          Price
          <input
            :value="Number(item.priceCents)"
            type="number"
            @input="item.priceCents = BigInt(($event.target as HTMLInputElement).value || 0)"
          />
        </label>
        <label>Currency<input v-model="item.currency" /></label>
      </div>
      <p class="muted" style="margin: 0 14px 14px; font-size: 12px">
        {{ formatPrice(item.priceCents, item.currency) }} · these are the only fields on
        <span class="mono">pim.items</span>. Nothing below is stored here.
      </p>
    </section>

    <!-- Read-side summary. Present mostly to make the architecture legible: the
         same question answered three ways, with the cost of each shown. -->
    <section class="card panel" style="margin-top: 20px">
      <header>
        <strong>Linked documents</strong>
        <span class="spacer" />
        <select v-model="strategy">
          <option v-for="o in strategyOptions" :key="o.value" :value="o.value">
            {{ o.label }}
          </option>
        </select>
      </header>

      <div v-if="links" class="links">
        <p class="muted mono" style="margin: 0 0 10px">
          {{ (links.totalMicros / 1000n > 0n ? Number(links.totalMicros) / 1000 : 0).toFixed(2) }} ms
          · {{ links.fanoutRpcs }} outbound RPC{{ links.fanoutRpcs === 1 ? "" : "s" }}
        </p>
        <div v-for="g in links.groups" :key="g.namespace + g.predicate" class="group">
          <span class="mono muted">{{ g.namespace }} · {{ g.predicate }}</span>
          <span v-if="g.error" class="mono" style="color: var(--danger)"> unavailable</span>
          <ul v-else>
            <li v-for="d in g.documents" :key="d.ref?.id">
              {{ d.title }}
              <span class="muted">{{ d.subtitle }}</span>
            </li>
            <li v-if="g.documents.length === 0" class="muted">none</li>
          </ul>
        </div>
      </div>
    </section>

    <!-- The extension point. -->
    <div class="slot-header">
      <h2>Cards</h2>
      <span class="mono muted">{{ ITEM_DETAIL_SLOT }}</span>
      <span class="spacer" />
      <select
        v-if="addable.length"
        :value="''"
        @change="addCard(($event.target as HTMLSelectElement).value)"
      >
        <option value="" disabled>Add card…</option>
        <option v-for="c in addable" :key="c.id" :value="c.id">
          {{ c.icon }} {{ c.title }}
        </option>
      </select>
      <span v-else class="muted">all cards shown</span>
    </div>

    <div class="cards">
      <EmbedCard
        v-for="c in mountedContributions"
        :key="c.id"
        :contribution="c"
        :anchor-ref="anchorRef"
        @invalidate="loadLinks"
        @navigate="(ref) => pushToast('info', `Card asked to navigate to ${ref}`, c.namespace)"
        @remove="removeCard(c.id)"
      />
      <p v-if="mountedContributions.length === 0" class="muted" style="padding: 8px 2px">
        No cards mounted. Any service that registers a contribution to this slot appears in
        the menu above without this application being rebuilt.
      </p>
    </div>
  </div>
</template>

<style scoped>
.panel header {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 14px;
  border-bottom: 1px solid var(--border);
  background: var(--surface-2);
}
.spacer { flex: 1; }

.grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 14px;
  padding: 16px 14px;
}
.grid label { display: flex; flex-direction: column; gap: 5px; font-size: 12px; color: var(--text-dim); }
.grid label input { color: var(--text); }
.grid .wide { grid-column: 1 / -1; }

.links { padding: 14px; }
.group { margin-bottom: 10px; }
.group ul { margin: 4px 0 0; padding-left: 18px; }

.slot-header {
  display: flex;
  align-items: baseline;
  gap: 10px;
  margin: 30px 0 14px;
}
.slot-header h2 { font-size: 14px; margin: 0; }

.cards { display: flex; flex-direction: column; gap: 16px; }
</style>
