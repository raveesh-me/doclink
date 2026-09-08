/**
 * The wire protocol between the PIM shell (host) and an embedded satellite card
 * (guest).
 *
 * This file is the entire contract. A satellite team needs to read this and
 * nothing else about the host: no component library, no build config, no
 * framework. The shipping card in this repo is plain TypeScript and the
 * subscriptions card is Vue, which is the demonstration that it holds.
 *
 * Design rules, in rough order of importance:
 *
 *  1. The host sends exactly one piece of domain data: the anchor's DocRef
 *     string. Not the item, not its fields, not a token scoped to it. If a card
 *     needs more, it asks its own backend using its own linkage tables.
 *  2. Messages are always targeted at a specific origin, never "*", and always
 *     validated on receipt by both origin and source window.
 *  3. Everything else in the protocol is presentation plumbing: height, theme,
 *     navigation requests, toasts. None of it carries domain meaning.
 */

export const PROTOCOL_VERSION = 1;

/** Messages the host sends to a guest. */
export type HostMessage =
  | {
      type: "doclink:host:init";
      version: number;
      /** Canonical DocRef string, e.g. "pim/item/01J8XYZ". The only domain data crossing the boundary. */
      anchorRef: string;
      /** Opaque id of this contribution, so a guest mounted twice can tell instances apart. */
      contributionId: string;
      theme: Theme;
      /** Host origin, so the guest can target its replies rather than broadcasting. */
      hostOrigin: string;
    }
  | { type: "doclink:host:theme"; theme: Theme }
  /** The host is asking a dirty guest to save before it navigates away. */
  | { type: "doclink:host:flush"; nonce: string };

/** Messages a guest sends to the host. */
export type GuestMessage =
  | { type: "doclink:embed:ready"; version: number }
  | { type: "doclink:embed:resize"; height: number }
  /** Ask the host to navigate. The guest cannot navigate the top window itself. */
  | { type: "doclink:embed:navigate"; docRef: string }
  | { type: "doclink:embed:toast"; level: "info" | "success" | "error"; message: string }
  /** Report unsaved changes so the host can warn before unmounting the card. */
  | { type: "doclink:embed:dirty"; dirty: boolean }
  | { type: "doclink:embed:flushed"; nonce: string }
  /**
   * The guest changed data the host may be summarizing elsewhere on the page.
   * The host reacts by re-reading its own view; it is never told what changed,
   * because it would not know what the change means.
   */
  | { type: "doclink:embed:invalidate" };

export type Theme = "light" | "dark";

export function isHostMessage(value: unknown): value is HostMessage {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as { type?: unknown }).type === "string" &&
    (value as { type: string }).type.startsWith("doclink:host:")
  );
}

export function isGuestMessage(value: unknown): value is GuestMessage {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as { type?: unknown }).type === "string" &&
    (value as { type: string }).type.startsWith("doclink:embed:")
  );
}

/** Extracts the origin from an absolute embed URL, for postMessage targeting. */
export function originOf(url: string): string {
  return new URL(url, window.location.href).origin;
}
