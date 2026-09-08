import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { SubscriptionsService } from "@doclink/gen/subscriptions/v1/subscriptions_pb.js";

declare global {
  interface Window {
    __SUBSCRIPTIONS_CONFIG__?: { subscriptions?: string };
  }
}

const baseUrl = window.__SUBSCRIPTIONS_CONFIG__?.subscriptions ?? "http://localhost:8082";

/**
 * One client, to our own backend. There is deliberately no PIM client here: if
 * this card ever needed to read the item, that would be a dependency from a
 * satellite onto the anchor's API, and the arrow is supposed to point the other
 * way.
 */
export const subscriptionsClient = createClient(
  SubscriptionsService,
  createConnectTransport({ baseUrl }),
);
