import { ref } from "vue";

export interface Toast {
  id: number;
  level: "info" | "success" | "error";
  message: string;
  /** Namespace of the card that raised it, so the user can tell who is talking. */
  source?: string;
}

export const toasts = ref<Toast[]>([]);
let nextId = 1;

export function pushToast(level: Toast["level"], message: string, source?: string) {
  const id = nextId++;
  toasts.value.push({ id, level, message, source });
  window.setTimeout(() => {
    toasts.value = toasts.value.filter((t) => t.id !== id);
  }, 4500);
}
