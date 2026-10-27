import { useEffect, useState, type ReactNode } from "react";
import { Menu, type MenuItem } from "./Menu";

export type { MenuItem };

type MenuState = { x: number; y: number; items: MenuItem[] } | null;

/**
 * Right-click context menu. Returns an `onContextMenu` handler factory and
 * renders the menu. The app-wide default browser menu is disabled globally
 * (see main.tsx); components opt in to their own via `menu(items)`.
 */
export function useContextMenu() {
  const [state, setState] = useState<MenuState>(null);

  useEffect(() => {
    if (!state) return;
    const close = () => setState(null);
    window.addEventListener("click", close);
    window.addEventListener("scroll", close, true);
    window.addEventListener("resize", close);
    return () => {
      window.removeEventListener("click", close);
      window.removeEventListener("scroll", close, true);
      window.removeEventListener("resize", close);
    };
  }, [state]);

  const menu =
    (items: MenuItem[]) => (e: React.MouseEvent) => {
      e.preventDefault();
      e.stopPropagation();
      setState({ x: e.clientX, y: e.clientY, items: items.filter(Boolean) });
    };

  // The same menu at a point, for the keyboard (Shift+F10 or the Menu key).
  const at = (items: MenuItem[], x: number, y: number) => setState({ x, y, items: items.filter(Boolean) });

  const node: ReactNode = state ? (
    <Menu items={state.items} x={state.x} y={state.y} onClose={() => setState(null)} />
  ) : null;

  return { menu, at, node };
}
