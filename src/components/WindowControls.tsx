import { Window as win } from "@wailsio/runtime";

// Window preferences, kept in Settings > General and applied at start-up.
// The size scales every rem, so UI zoom is one font size on the root.
export const windowPrefs = {
  zoom: () => Number(localStorage.getItem("uiZoom")) || 100,
  onTop: () => localStorage.getItem("alwaysOnTop") === "1",
};
export function setZoom(z: number) {
  document.documentElement.style.fontSize = `${z}%`;
  localStorage.setItem("uiZoom", String(z));
}
export function setAlwaysOnTop(on: boolean) {
  win.SetAlwaysOnTop(on).catch(() => {});
  localStorage.setItem("alwaysOnTop", on ? "1" : "0");
}
export function applyWindowPrefs() {
  setZoom(windowPrefs.zoom());
  if (windowPrefs.onTop()) setAlwaysOnTop(true);
}
