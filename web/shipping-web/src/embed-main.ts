/**
 * The shipping card: the same contract as the subscriptions card, implemented
 * with no framework at all.
 *
 * This file is the control in the experiment. If the host protocol had leaked a
 * framework assumption — a shared component base class, a store, a build-time
 * import from the shell — this card could not exist. It gets the same anchor
 * DocRef through the same postMessage handshake and renders with document
 * methods.
 */
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { connectToHost, type GuestBridge } from "@doclink/host-sdk/guest";
import { ShippingService } from "@doclink/gen/shipping/v1/shipping_pb.js";
import type {
  Assignment,
  Profile,
} from "@doclink/gen/shipping/v1/shipping_pb.js";
import "./embed.css";

declare global {
  interface Window {
    __SHIPPING_CONFIG__?: { shipping?: string };
  }
}

const client = createClient(
  ShippingService,
  createConnectTransport({
    baseUrl: window.__SHIPPING_CONFIG__?.shipping ?? "http://localhost:8083",
  }),
);

const root = document.getElementById("card")!;

connectToHost()
  .then((bridge) => {
    applyTheme(bridge.context.theme);
    bridge.onThemeChange(applyTheme);
    void render(bridge, bridge.context.anchorRef);
    bridge.autoResize(document.body);
  })
  .catch((err: Error) => {
    root.innerHTML = `<div class="standalone">
      <strong>Shipping card</strong>
      <p>This page is an embedded card and has no meaning on its own.</p>
      <code></code></div>`;
    root.querySelector("code")!.textContent = err.message;
  });

function applyTheme(theme: string) {
  document.documentElement.dataset.theme = theme;
}

function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Record<string, string> = {},
  ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") node.className = v;
    else node.setAttribute(k, v);
  }
  for (const c of children) node.append(c);
  return node;
}

async function render(bridge: GuestBridge, anchorRef: string) {
  root.replaceChildren(el("p", { class: "muted body" }, "Loading shipping…"));

  let assignment: Assignment | undefined;
  let profile: Profile | undefined;
  let profiles: Profile[] = [];

  try {
    const res = await client.getAssignmentForAnchor({ anchorRef });
    assignment = res.assignment;
    profile = res.profile;
    profiles = res.availableProfiles;
  } catch (e) {
    root.replaceChildren(
      el("p", { class: "muted body" }, `Could not reach shipping: ${String(e)}`),
    );
    return;
  }

  const body = el("div", { class: "body" });

  if (assignment && profile) {
    body.append(
      el(
        "div",
        { class: "current" },
        el("strong", {}, profile.name),
        el(
          "span",
          { class: "muted" },
          ` ${profile.carrier} · ${profile.handlingDays}d handling` +
            (profile.requiresSignature ? " · signature" : ""),
        ),
        el(
          "div",
          { class: "muted mono meta" },
          `${assignment.weightGrams} g · ${assignment.dimensionsCm || "no dims"}` +
            (assignment.hazmat ? " · HAZMAT" : ""),
        ),
      ),
    );
  } else {
    body.append(el("p", { class: "muted" }, "No shipping profile assigned to this item."));
  }

  // Assignment form. Fields here (weight, dimensions, hazmat) are attributes of
  // the item that only shipping cares about, which is exactly the data that
  // would otherwise have been pushed into pim.items as four more columns.
  const select = el("select", {});
  for (const p of profiles) {
    const opt = el("option", { value: p.id }, `${p.name} — ${p.carrier}`);
    if (assignment?.profileId === p.id) opt.setAttribute("selected", "selected");
    select.append(opt);
  }

  const weight = el("input", {
    type: "number",
    min: "0",
    max: "100000",
    style: "width:88px",
    value: String(assignment?.weightGrams ?? 0),
  });
  const dims = el("input", {
    placeholder: "LxWxH",
    style: "width:96px",
    value: assignment?.dimensionsCm ?? "",
  });
  const hazmat = el("input", { type: "checkbox" });
  if (assignment?.hazmat) hazmat.setAttribute("checked", "checked");

  // Any edit marks the card dirty, so the host can warn before unmounting it.
  for (const input of [select, weight, dims, hazmat]) {
    input.addEventListener("input", () => bridge.setDirty(true));
  }

  const save = el("button", { class: "primary" }, assignment ? "Update" : "Assign");
  save.addEventListener("click", async () => {
    save.setAttribute("disabled", "true");
    try {
      await client.assignProfile({
        anchorRef,
        profileId: select.value,
        weightGrams: Number(weight.value) || 0,
        dimensionsCm: dims.value,
        hazmat: hazmat.checked,
      });
      bridge.setDirty(false);
      bridge.toast("success", "Shipping profile saved");
      bridge.invalidate();
      await render(bridge, anchorRef);
    } catch (e) {
      bridge.toast("error", e instanceof Error ? e.message : String(e));
      save.removeAttribute("disabled");
    }
  });

  const controls = el(
    "div",
    { class: "controls" },
    select,
    el("label", { class: "muted" }, "weight g", weight),
    el("label", { class: "muted" }, "dims", dims),
    el("label", { class: "muted" }, hazmat, "hazmat"),
    save,
  );

  if (assignment) {
    const clear = el("button", { class: "link" }, "Unassign");
    clear.addEventListener("click", async () => {
      await client.unassignProfile({ anchorRef });
      bridge.setDirty(false);
      bridge.toast("info", "Shipping profile removed");
      bridge.invalidate();
      await render(bridge, anchorRef);
    });
    controls.append(clear);
  }

  body.append(controls);
  body.append(
    el(
      "p",
      { class: "muted mono footnote" },
      `shipping.assignments keyed on ${anchorRef} — rendered without a framework`,
    ),
  );

  root.replaceChildren(body);

  // The host sizes us from this. Re-reported after every re-render because the
  // card's height changes with its content.
  bridge.reportHeight(document.body.getBoundingClientRect().height);
}
