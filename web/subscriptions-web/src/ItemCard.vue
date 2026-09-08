<script setup lang="ts">
/**
 * The subscriptions card, as it appears inside the PIM item screen.
 *
 * Its only input is `anchorRef`, a string like "pim/item/01J8XYZ". It resolves
 * everything else through subscriptions' own coverage table, keyed on that
 * string. PIM stores no plan id, and this component reads no PIM data.
 */
import { ref, onMounted, computed } from "vue";
import { subscriptionsClient } from "./api.js";
import type { Coverage, Plan } from "@doclink/gen/subscriptions/v1/subscriptions_pb.js";
import type { GuestBridge } from "@doclink/host-sdk/guest";

const props = defineProps<{ anchorRef: string; bridge: GuestBridge }>();

const coverage = ref<Coverage[]>([]);
const plans = ref<Plan[]>([]);
const loading = ref(true);
const error = ref("");

const selectedPlan = ref("");
const maxQty = ref(1);
const prepaidOnly = ref(false);

const planById = computed(() => new Map(plans.value.map((p) => [p.id, p])));
const attachable = computed(() => {
  const taken = new Set(coverage.value.map((c) => c.planId));
  return plans.value.filter((p) => !taken.has(p.id));
});

async function load() {
  loading.value = true;
  error.value = "";
  try {
    const res = await subscriptionsClient.listCoverageForAnchor({ anchorRef: props.anchorRef });
    coverage.value = res.coverage;
    plans.value = res.plans;
    selectedPlan.value = attachable.value[0]?.id ?? "";
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e);
  } finally {
    loading.value = false;
  }
}

async function attach() {
  if (!selectedPlan.value) return;
  try {
    await subscriptionsClient.attachPlan({
      anchorRef: props.anchorRef,
      planId: selectedPlan.value,
      maxQuantityPerCycle: maxQty.value,
      prepaidOnly: prepaidOnly.value,
    });
    props.bridge.toast("success", "Plan attached");
    // Tell the host its own summaries are stale. We do not say what changed:
    // the host would not know what a plan is.
    props.bridge.invalidate();
    await load();
  } catch (e) {
    props.bridge.toast("error", e instanceof Error ? e.message : String(e));
  }
}

async function detach(id: string) {
  try {
    await subscriptionsClient.detachPlan({ coverageId: id });
    props.bridge.toast("info", "Plan detached");
    props.bridge.invalidate();
    await load();
  } catch (e) {
    props.bridge.toast("error", e instanceof Error ? e.message : String(e));
  }
}

function discount(bps: number) {
  return `${(bps / 100).toFixed(2)}%`;
}

onMounted(load);
</script>

<template>
  <div class="body">
    <p v-if="loading" class="muted">Loading coverage…</p>
    <p v-else-if="error" class="muted">Could not reach subscriptions: {{ error }}</p>

    <template v-else>
      <ul v-if="coverage.length" class="rows">
        <li v-for="c in coverage" :key="c.id">
          <div class="row-main">
            <strong>{{ planById.get(c.planId)?.name ?? c.planId }}</strong>
            <span class="muted">
              {{ planById.get(c.planId)?.interval }} ·
              {{ discount(planById.get(c.planId)?.discountBps ?? 0) }} off
            </span>
          </div>
          <div class="row-meta muted mono">
            max {{ c.maxQuantityPerCycle }}/cycle{{ c.prepaidOnly ? " · prepaid only" : "" }}
          </div>
          <button class="link" @click="detach(c.id)">Remove</button>
        </li>
      </ul>
      <p v-else class="muted">This item is not on any subscription plan.</p>

      <div v-if="attachable.length" class="attach">
        <select v-model="selectedPlan">
          <option v-for="p in attachable" :key="p.id" :value="p.id">
            {{ p.name }} — {{ discount(p.discountBps) }}
          </option>
        </select>
        <label class="muted">
          max/cycle
          <input v-model.number="maxQty" type="number" min="1" max="99" style="width: 62px" />
        </label>
        <label class="muted">
          <input v-model="prepaidOnly" type="checkbox" />
          prepaid only
        </label>
        <button class="primary" @click="attach">Attach</button>
      </div>

      <p class="muted mono footnote">
        subscriptions.coverage keyed on {{ anchorRef }} — no foreign key to pim.items
      </p>
    </template>
  </div>
</template>

<style scoped>
.body { padding: 14px; }
.rows { list-style: none; margin: 0 0 12px; padding: 0; }
.rows li {
  display: grid;
  grid-template-columns: 1fr auto auto;
  align-items: center;
  gap: 10px;
  padding: 8px 0;
  border-bottom: 1px solid var(--border);
}
.rows li:last-child { border-bottom: 0; }
.row-main { display: flex; flex-direction: column; }
.row-meta { font-size: 11px; }

.attach {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  padding-top: 10px;
  border-top: 1px solid var(--border);
}
.attach label { display: inline-flex; align-items: center; gap: 5px; font-size: 12px; }

.footnote { margin: 12px 0 0; font-size: 11px; }
</style>
