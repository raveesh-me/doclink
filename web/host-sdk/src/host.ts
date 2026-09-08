/**
 * Host side of the embed protocol. Framework-agnostic on purpose: the Vue shell
 * wraps this in a component, but nothing here knows about Vue.
 */
import {
  PROTOCOL_VERSION,
  isGuestMessage,
  originOf,
  type GuestMessage,
  type HostMessage,
  type Theme,
} from "./protocol.js";

export interface MountOptions {
  /** Absolute URL of the guest document, as registered in doclink. */
  embedUrl: string;
  /** Canonical DocRef string of the anchor. The only domain data we hand over. */
  anchorRef: string;
  contributionId: string;
  theme: Theme;
  container: HTMLElement;

  onResize?: (height: number) => void;
  onNavigate?: (docRef: string) => void;
  onToast?: (level: "info" | "success" | "error", message: string) => void;
  onDirty?: (dirty: boolean) => void;
  onInvalidate?: () => void;
  onError?: (message: string) => void;
}

export interface MountedEmbed {
  iframe: HTMLIFrameElement;
  setTheme: (theme: Theme) => void;
  /** Ask a dirty guest to save; resolves when it acknowledges or the timeout expires. */
  flush: (timeoutMs?: number) => Promise<boolean>;
  destroy: () => void;
  readonly ready: Promise<void>;
}

/**
 * Mounts a satellite's card into `container`.
 *
 * The sandbox attribute is the load-bearing line in this file. allow-scripts and
 * allow-same-origin together would let the guest reach back into the host if it
 * were same-origin, so the deployment must serve every satellite from a distinct
 * origin; the kind manifests give each one its own hostname for exactly this
 * reason. allow-forms and allow-popups are omitted because a card that needs
 * either is doing something the host should know about.
 */
export function mountEmbed(opts: MountOptions): MountedEmbed {
  const guestOrigin = originOf(opts.embedUrl);
  if (guestOrigin === window.location.origin) {
    // Not fatal, but the isolation the design claims is not actually present,
    // and silently pretending otherwise would be the worst outcome.
    opts.onError?.(
      `Embed ${opts.embedUrl} is same-origin with the host; iframe isolation is not in effect.`,
    );
  }

  const iframe = document.createElement("iframe");
  iframe.src = opts.embedUrl;
  iframe.title = `Embedded card: ${opts.contributionId}`;
  iframe.setAttribute("sandbox", "allow-scripts allow-same-origin");
  iframe.setAttribute("loading", "lazy");
  iframe.style.width = "100%";
  iframe.style.border = "0";
  iframe.style.display = "block";
  // Provisional height until the guest reports its own. Chosen to be roughly a
  // card so the layout does not visibly jump on the common path.
  iframe.style.height = "180px";

  let destroyed = false;
  let resolveReady: () => void = () => {};
  let rejectReady: (e: Error) => void = () => {};
  const ready = new Promise<void>((resolve, reject) => {
    resolveReady = resolve;
    rejectReady = reject;
  });

  const pendingFlushes = new Map<string, (ok: boolean) => void>();

  function send(message: HostMessage) {
    if (destroyed || !iframe.contentWindow) return;
    // Targeted, never "*": a wildcard would leak the anchor ref to whatever
    // happens to be loaded in the frame if the guest were ever redirected.
    iframe.contentWindow.postMessage(message, guestOrigin);
  }

  function onMessage(event: MessageEvent) {
    // Both checks are required. Origin alone would accept a message from any
    // document on the guest's origin, including one in a different frame.
    if (event.origin !== guestOrigin) return;
    if (event.source !== iframe.contentWindow) return;
    if (!isGuestMessage(event.data)) return;

    const msg = event.data as GuestMessage;
    switch (msg.type) {
      case "doclink:embed:ready":
        if (msg.version !== PROTOCOL_VERSION) {
          opts.onError?.(
            `Embed speaks protocol v${msg.version}, host speaks v${PROTOCOL_VERSION}.`,
          );
        }
        send({
          type: "doclink:host:init",
          version: PROTOCOL_VERSION,
          anchorRef: opts.anchorRef,
          contributionId: opts.contributionId,
          theme: opts.theme,
          hostOrigin: window.location.origin,
        });
        resolveReady();
        break;
      case "doclink:embed:resize": {
        const h = Math.max(60, Math.min(4000, Math.round(msg.height)));
        iframe.style.height = `${h}px`;
        opts.onResize?.(h);
        break;
      }
      case "doclink:embed:navigate":
        opts.onNavigate?.(msg.docRef);
        break;
      case "doclink:embed:toast":
        opts.onToast?.(msg.level, msg.message);
        break;
      case "doclink:embed:dirty":
        opts.onDirty?.(msg.dirty);
        break;
      case "doclink:embed:invalidate":
        opts.onInvalidate?.();
        break;
      case "doclink:embed:flushed": {
        pendingFlushes.get(msg.nonce)?.(true);
        pendingFlushes.delete(msg.nonce);
        break;
      }
    }
  }

  window.addEventListener("message", onMessage);
  opts.container.appendChild(iframe);

  // A guest that never handshakes is a real failure mode: wrong URL, a crashed
  // pod, a CSP block. Surface it rather than leaving an empty box on the page.
  const readyTimer = window.setTimeout(() => {
    rejectReady(new Error(`Embed ${opts.embedUrl} did not complete handshake in 10s`));
    opts.onError?.(`Card did not load: ${opts.embedUrl}`);
  }, 10_000);
  ready.then(() => window.clearTimeout(readyTimer)).catch(() => {});

  return {
    iframe,
    ready,
    setTheme: (theme: Theme) => send({ type: "doclink:host:theme", theme }),
    flush: (timeoutMs = 3000) =>
      new Promise<boolean>((resolve) => {
        const nonce = Math.random().toString(36).slice(2);
        pendingFlushes.set(nonce, resolve);
        send({ type: "doclink:host:flush", nonce });
        window.setTimeout(() => {
          if (pendingFlushes.delete(nonce)) resolve(false);
        }, timeoutMs);
      }),
    destroy: () => {
      destroyed = true;
      window.clearTimeout(readyTimer);
      window.removeEventListener("message", onMessage);
      iframe.remove();
    },
  };
}
