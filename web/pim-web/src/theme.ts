import { ref, watch } from "vue";
import type { Theme } from "@doclink/host-sdk/protocol";

const stored = (() => {
  try {
    return localStorage.getItem("doclink-theme") as Theme | null;
  } catch {
    // Private windows and blocked site data both throw here. A theme
    // preference is not worth breaking the page over.
    return null;
  }
})();

export const theme = ref<Theme>(
  stored ?? (window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light"),
);

watch(
  theme,
  (value) => {
    document.documentElement.dataset.theme = value;
    try {
      localStorage.setItem("doclink-theme", value);
    } catch {
      /* ignore */
    }
  },
  { immediate: true },
);

export function toggleTheme() {
  theme.value = theme.value === "dark" ? "light" : "dark";
}
