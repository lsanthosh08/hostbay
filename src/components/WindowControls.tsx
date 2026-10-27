import { useEffect, useState } from "react";
import { Window as win } from "@wailsio/runtime";
import { t } from "../i18n";
import { IconButton } from "../ui/IconButton";
import { WindowIcon } from "../ui/icons";
import { useContextMenu } from "../ui/contextmenu";

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

// Window-adjust menu:
// minimize, maximize-restore and full screen. Zoom and always-on-top are
// settings (Settings > General).
export function WindowControls() {
  const { at, node } = useContextMenu();
  const [maximized, setMaximized] = useState(false);
  const [fullscreen, setFullscreen] = useState(false);

  useEffect(() => {
    win.IsMaximised().then(setMaximized).catch(() => {});
    win.IsFullscreen().then(setFullscreen).catch(() => {});
  }, []);

  const toggleMax = async () => {
    await win.ToggleMaximise();
    setMaximized(await win.IsMaximised());
  };
  const toggleFull = async () => {
    const next = !fullscreen;
    await (next ? win.Fullscreen() : win.UnFullscreen());
    setFullscreen(next);
  };

  const items = () => [
    { label: t("window.minimize"), onClick: () => void win.Minimise() },
    { label: maximized ? t("window.restore") : t("window.maximize"), onClick: () => void toggleMax() },
    { label: fullscreen ? t("window.exitFullScreen") : t("window.fullScreen"), onClick: () => void toggleFull() },
  ];

  return (
    <>
      <IconButton
        label={t("window.menu")}
        icon={WindowIcon}
        aria-haspopup="menu"
        onClick={(e) => {
          // The click must not reach the window listener that closes the menu.
          e.stopPropagation();
          // Beside the button, as the old popover sat; Menu keeps it on screen.
          const r = e.currentTarget.getBoundingClientRect();
          at(items(), r.right + 8, r.top);
        }}
      />
      {node}
    </>
  );
}
