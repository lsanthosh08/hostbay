import { useSyncExternalStore } from "react";
import * as api from "../api";
import type { Vpn, VpnState } from "../api";
import { t, type MessageKey } from "../i18n";

export type VpnStates = Record<string, VpnState>;

// One copy of the profiles and their live states for every view that shows
// them (panel, tabs, host menus, palette). It subscribes on first use and
// stays subscribed for the app's life, as the backend's states do.
let snap: { vpns: Vpn[]; states: VpnStates } = { vpns: [], states: {} };
const subs = new Set<() => void>();
let started = false;
let loaded = false;
const emit = (next: typeof snap) => {
  snap = next;
  subs.forEach((f) => f());
};
/** Reads the saved profiles again, after a save or delete. */
export const reloadVpns = () =>
  api
    .listVpns()
    .then((vpns) => {
      // An unchanged list keeps its array, so its readers do not refetch.
      if (loaded && JSON.stringify(vpns) === JSON.stringify(snap.vpns)) return;
      loaded = true;
      emit({ ...snap, vpns });
    })
    .catch(() => {});
function subscribe(cb: () => void) {
  subs.add(cb);
  if (!started) {
    started = true;
    reloadVpns();
    // An event that arrived first is newer than the list.
    api
      .vpnStates()
      .then((all) => emit({ ...snap, states: { ...Object.fromEntries(all.map((s) => [s.id, s])), ...snap.states } }))
      .catch(() => {});
    api.onVpnState((s) => emit({ ...snap, states: { ...snap.states, [s.id]: s } }));
  }
  return () => {
    subs.delete(cb);
  };
}
/** The saved profiles and every known live state (unknown is disconnected). */
export const useVpns = () => useSyncExternalStore(subscribe, () => snap);

/** On its way up, or up: Disconnect rather than Connect. */
export const isVpnActive = (s?: VpnState) => !!s && (s.status === "connecting" || s.status === "signing_in" || s.status === "connected");

const STATUS: Record<api.VpnStatus, MessageKey> = {
  disconnected: "vpn.status.disconnected",
  connecting: "vpn.status.connecting",
  signing_in: "vpn.status.signingIn",
  connected: "vpn.status.connected",
  failed: "vpn.status.failed",
  dropped: "vpn.status.dropped",
};
export const vpnStatusText = (s?: VpnState) => t(STATUS[s?.status ?? "disconnected"] ?? "vpn.status.disconnected");

/** A connect the user ended on purpose (disconnect while connecting) is not a failure to report. */
export function vpnCancelled(e: unknown): boolean {
  const { code, details } = api.errorInfo(e);
  return code === "VPNFailed" && details?.reason === "disconnected";
}
