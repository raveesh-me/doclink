<script setup lang="ts">
/**
 * A single satellite card on the item detail screen.
 *
 * Everything this component knows about the satellite came from a registry row
 * fetched at page load: a title, an icon, and a URL. It has no import from the
 * satellite, no type for its data, and no idea what the card will render. It
 * hands over one DocRef string and gets back a rectangle.
 */
import { ref, onMounted, onBeforeUnmount, watch } from "vue";
import { mountEmbed, type MountedEmbed } from "@doclink/host-sdk/host";
import type { Contribution } from "@doclink/gen/doclink/v1/registry_pb.js";
import { theme } from "../theme.js";
import { pushToast } from "../toasts.js";

const props = defineProps<{
  contribution: Contribution;
  anchorRef: string;
}>();

const emit = defineEmits<{
  (e: "invalidate"): void;
  (e: "navigate", docRef: string): void;
  (e: "remove"): void;
}>();

const container = ref<HTMLElement | null>(null);
const status = ref<"loading" | "ready" | "failed">("loading");
const failure = ref("");
const dirty = ref(false);
let embed: MountedEmbed | null = null;

onMounted(() => {
  if (!container.value) return;
  embed = mountEmbed({
    embedUrl: props.contribution.embedUrl,
    anchorRef: props.anchorRef,
    contributionId: props.contribution.id,
    theme: theme.value,
    container: container.value,
    onNavigate: (docRef) => emit("navigate", docRef),
    onInvalidate: () => emit("invalidate"),
    onDirty: (d) => (dirty.value = d),
    onToast: (level, message) => pushToast(level, message, props.contribution.namespace),
    onError: (message) => {
      status.value = "failed";
      failure.value = message;
    },
  });
  embed.ready.then(() => (status.value = "ready")).catch(() => {});
});

// Theme is pushed, not inherited: the iframe is a separate document and cannot
// see the host's CSS variables.
watch(theme, (value) => embed?.setTheme(value));

onBeforeUnmount(() => embed?.destroy());

async function remove() {
  if (dirty.value) {
    const ok = window.confirm(
      `${props.contribution.title} has unsaved changes. Remove the card anyway?`,
    );
    if (!ok) return;
  }
  emit("remove");
}
</script>

<template>
  <section class="card embed">
    <header>
      <span class="icon">{{ contribution.icon || "◻︎" }}</span>
      <strong>{{ contribution.title }}</strong>
      <span class="mono muted ns">{{ contribution.namespace }}</span>
      <span v-if="dirty" class="mono muted">· unsaved</span>
      <span class="spacer" />
      <button class="ghost" title="Remove this card from the screen" @click="remove">✕</button>
    </header>

    <p v-if="status === 'failed'" class="failure muted">
      {{ failure }}
      <br />
      <span class="mono">{{ contribution.embedUrl }}</span>
    </p>

    <!-- The iframe is appended here by the host SDK. It stays in the DOM even
         when the handshake fails, so a slow card can still arrive late. -->
    <div ref="container" class="frame" :class="{ hidden: status === 'failed' }" />

    <div v-if="status === 'loading'" class="skeleton muted">Loading card…</div>
  </section>
</template>

<style scoped>
.embed { overflow: hidden; }

header {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  border-bottom: 1px solid var(--border);
  background: var(--surface-2);
}
.icon { font-size: 15px; }
.ns { font-size: 11px; }
.spacer { flex: 1; }
.ghost {
  border-color: transparent;
  background: transparent;
  padding: 2px 7px;
  color: var(--text-dim);
}
.frame { position: relative; }
.frame.hidden { display: none; }
.skeleton { padding: 22px 14px; }
.failure { padding: 14px; margin: 0; }
</style>
