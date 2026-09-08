<script setup lang="ts">
import { RouterView, RouterLink } from "vue-router";
import { theme, toggleTheme } from "./theme.js";
import { toasts } from "./toasts.js";
</script>

<template>
  <div class="app">
    <header class="topbar">
      <RouterLink to="/items" style="text-decoration: none">
        <h1>Product Information Management</h1>
      </RouterLink>
      <span class="tag">doc-link POC</span>
      <span class="spacer" />
      <button @click="toggleTheme" :title="`Switch to ${theme === 'dark' ? 'light' : 'dark'} theme`">
        {{ theme === "dark" ? "☀︎" : "☾" }}
      </button>
    </header>

    <RouterView />

    <!-- Toasts raised by embedded cards arrive over postMessage and are
         rendered by the host, so a card cannot draw outside its own frame. -->
    <div class="toast-stack">
      <div v-for="t in toasts" :key="t.id" class="toast" :class="t.level">
        <strong v-if="t.source" class="mono">{{ t.source }}</strong>
        <span v-if="t.source"> · </span>{{ t.message }}
      </div>
    </div>
  </div>
</template>
