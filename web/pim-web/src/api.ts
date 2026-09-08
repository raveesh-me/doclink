import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { ItemService } from "@doclink/gen/pim/v1/item_pb.js";
import { RegistryService } from "@doclink/gen/doclink/v1/registry_pb.js";
import { config } from "./config.js";

/**
 * The shell talks to exactly two backends: its own (PIM) and the registry.
 *
 * It has no client for subscriptions or shipping and no URL for either. Those
 * are reached only by their own embedded cards, over their own origins. If this
 * file ever grows a third client for a satellite, the decoupling has been lost.
 */
export const itemClient = createClient(
  ItemService,
  createConnectTransport({ baseUrl: config.pim }),
);

export const registryClient = createClient(
  RegistryService,
  createConnectTransport({ baseUrl: config.doclink }),
);

/** The extension point this application declares and renders. */
export const ITEM_DETAIL_SLOT = "pim/item:detail.card";

export function itemDocRef(id: string): string {
  return `pim/item/${id}`;
}

export function formatPrice(cents: bigint | number, currency: string): string {
  const value = Number(cents) / 100;
  try {
    return new Intl.NumberFormat(undefined, { style: "currency", currency }).format(value);
  } catch {
    return `${currency} ${value.toFixed(2)}`;
  }
}
