/**
 * Guest side of the embed protocol, for satellite teams.
 *
 * A card's whole integration with the host is:
 *
 *   const ctx = await connectToHost();
 *   // ctx.anchorRef is "pim/item/01J8XYZ" and is all you get
 *
 * followed by calls to your own backend. You never import anything from the
 * host application, and the host never imports anything from you.
 */
import {
  PROTOCOL_VERSION,
  isHostMessage,
  type GuestMessage,
  type HostMessage,
  type Theme,
} from "./protocol.js";

export interface HostContext {
  /** Canonical DocRef of the anchor document, e.g. "pim/item/01J8XYZ". */
  anchorRef: string;
  contributionId: string;
  theme: Theme;
  hostOrigin: string;
}

export interface GuestBridge {
  context: HostContext;
  /** Called whenever the host changes theme. */
  onThemeChange: (fn: (theme: Theme) => void) => void;
  /** Called when the host wants unsaved work committed; return when done. */
  onFlush: (fn: () => Promise<void> | void) => void;
  /** Start reporting our document height to the host. Idempotent. */
  autoResize: (el?: HTMLElement) => void;
  reportHeight: (height: number) => void;
  navigate: (docRef: string) => void;
  toast: (level: "info" | "success" | "error", message: string) => void;
  setDirty: (dirty: boolean) => void;
  /** Tell the host our data changed so it can refresh its own summaries. */
  invalidate: () => void;
}

/**
 * Completes the handshake and resolves with the anchor reference.
 *
 * Rejects if no host answers, which is the correct behaviour: a card opened
 * directly in a browser tab has no anchor and cannot render anything meaningful.
 */
export function connectToHost(timeoutMs = 10_000): Promise<GuestBridge> {
  return new Promise((resolve, reject) => {
    let hostOrigin: string | null = null;
    let themeHandler: ((theme: Theme) => void) | null = null;
    let flushHandler: (() => Promise<void> | void) | null = null;
    let resizeObserver: ResizeObserver | null = null;
    let lastHeight = -1;

    function send(message: GuestMessage) {
      if (!hostOrigin) return;
      window.parent.postMessage(message, hostOrigin);
    }

    function reportHeight(height: number) {
      const rounded = Math.ceil(height);
      // Guard against feedback loops: a resize that changes the iframe height
      // can re-trigger the observer, and without this the two sides can
      // oscillate by a pixel forever.
      if (Math.abs(rounded - lastHeight) < 2) return;
      lastHeight = rounded;
      send({ type: "doclink:embed:resize", height: rounded });
    }

    const timer = window.setTimeout(() => {
      window.removeEventListener("message", onMessage);
      reject(new Error("No doc-link host responded; is this card embedded?"));
    }, timeoutMs);

    function onMessage(event: MessageEvent) {
      if (!isHostMessage(event.data)) return;
      // The host's origin is learned from the first init message rather than
      // configured, because a satellite must not have to know, or maintain, a
      // list of hosts that embed it.
      if (event.source !== window.parent) return;
      const msg = event.data as HostMessage;

      if (msg.type === "doclink:host:init") {
        if (hostOrigin) return; // already initialised; ignore replays
        hostOrigin = event.origin;
        window.clearTimeout(timer);

        if (msg.version !== PROTOCOL_VERSION) {
          console.warn(
            `doc-link: host protocol v${msg.version}, guest v${PROTOCOL_VERSION}`,
          );
        }

        resolve({
          context: {
            anchorRef: msg.anchorRef,
            contributionId: msg.contributionId,
            theme: msg.theme,
            hostOrigin: event.origin,
          },
          onThemeChange: (fn) => {
            themeHandler = fn;
          },
          onFlush: (fn) => {
            flushHandler = fn;
          },
          autoResize: (el) => {
            const target = el ?? document.documentElement;
            resizeObserver?.disconnect();
            resizeObserver = new ResizeObserver(() => {
              reportHeight(target.getBoundingClientRect().height);
            });
            resizeObserver.observe(target);
            reportHeight(target.getBoundingClientRect().height);
          },
          reportHeight,
          navigate: (docRef) => send({ type: "doclink:embed:navigate", docRef }),
          toast: (level, message) => send({ type: "doclink:embed:toast", level, message }),
          setDirty: (dirty) => send({ type: "doclink:embed:dirty", dirty }),
          invalidate: () => send({ type: "doclink:embed:invalidate" }),
        });
        return;
      }

      if (event.origin !== hostOrigin) return;

      if (msg.type === "doclink:host:theme") {
        themeHandler?.(msg.theme);
      } else if (msg.type === "doclink:host:flush") {
        const nonce = msg.nonce;
        Promise.resolve(flushHandler?.()).finally(() =>
          send({ type: "doclink:embed:flushed", nonce }),
        );
      }
    }

    window.addEventListener("message", onMessage);
    // The host waits for this before sending init, so a guest that loads slowly
    // still gets its anchor.
    window.parent.postMessage(
      { type: "doclink:embed:ready", version: PROTOCOL_VERSION } satisfies GuestMessage,
      "*", // We do not know the host origin yet; this message carries no data.
    );
  });
}
