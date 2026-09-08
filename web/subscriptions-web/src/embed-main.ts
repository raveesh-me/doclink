/**
 * Entry point for the subscriptions card.
 *
 * The whole integration is the connectToHost() call: it yields one string, the
 * anchor DocRef. Everything after that is ordinary application code talking to
 * the subscriptions backend.
 */
import { createApp } from "vue";
import { connectToHost } from "@doclink/host-sdk/guest";
import ItemCard from "./ItemCard.vue";
import "./embed.css";

const mountPoint = document.getElementById("card")!;

connectToHost()
  .then((bridge) => {
    document.documentElement.dataset.theme = bridge.context.theme;
    bridge.onThemeChange((theme) => {
      document.documentElement.dataset.theme = theme;
    });

    createApp(ItemCard, {
      anchorRef: bridge.context.anchorRef,
      bridge,
    }).mount(mountPoint);

    // The host sizes the iframe from what we report; nothing else can.
    bridge.autoResize(document.body);
  })
  .catch((err: Error) => {
    // Opened directly rather than embedded. Say so plainly instead of rendering
    // an empty card: without a host there is no anchor, and without an anchor
    // this page has nothing to show.
    mountPoint.innerHTML = `<div class="standalone">
      <strong>Subscriptions card</strong>
      <p>This page is an embedded card and has no meaning on its own.
      It renders when a host passes it an item reference.</p>
      <code>${err.message}</code>
    </div>`;
  });
