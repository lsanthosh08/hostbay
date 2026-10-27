import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import { CollapsedIcon, type Icon } from "./icons";
import { Kbd } from "./Kbd";
import { layer } from "./layers";

export type MenuItem = {
  label: string;
  onClick?: () => void;
  danger?: boolean;
  disabled?: boolean;
  icon?: Icon;
  shortcut?: string;
  /** A submenu, opened by hover, click or ArrowRight. */
  items?: MenuItem[];
};

/**
 * A menu at a screen point, in a portal. Arrow keys, Home and End move
 * between items; Enter or Space picks one; ArrowRight opens a submenu and
 * ArrowLeft or Escape closes it; Escape or Tab closes the menu, and focus
 * returns to where it was.
 */
export function Menu({
  items,
  x,
  y,
  onClose,
  onBack,
}: {
  items: MenuItem[];
  x: number;
  y: number;
  onClose: () => void;
  /** Set on a submenu: close just this level. */
  onBack?: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState({ left: x, top: y });
  const [sub, setSub] = useState<{ index: number; x: number; y: number } | null>(null);
  // Read before the layout effect below moves focus into the menu.
  const [before] = useState(() => document.activeElement as HTMLElement | null);

  const buttons = () => [...ref.current!.querySelectorAll<HTMLButtonElement>("[role=menuitem]:not(:disabled)")];

  // Keep the menu on screen, then focus its first item.
  useLayoutEffect(() => {
    const r = ref.current!.getBoundingClientRect();
    setPos({
      left: Math.max(4, Math.min(x, window.innerWidth - r.width - 4)),
      top: Math.max(4, Math.min(y, window.innerHeight - r.height - 4)),
    });
    buttons()[0]?.focus();
  }, [x, y]);

  // The top level hands focus back when it closes.
  useEffect(() => (onBack ? undefined : () => before?.focus()), [onBack, before]);

  const openSub = (index: number, el: HTMLElement) => {
    const r = el.getBoundingClientRect();
    setSub({ index, x: r.right - 4, y: r.top - 4 });
  };
  const pick = (it: MenuItem, index: number, el: HTMLElement) => {
    if (it.items) return openSub(index, el);
    onClose();
    it.onClick?.();
  };

  const onKeyDown = (e: KeyboardEvent) => {
    // This level owns the keys; a parent menu must not act on them too.
    e.stopPropagation();
    const list = buttons();
    const at = list.indexOf(document.activeElement as HTMLButtonElement);
    const move = (i: number) => list[(i + list.length) % list.length]?.focus();
    const it = items[Number((document.activeElement as HTMLElement)?.dataset.index)];
    if (e.key === "ArrowDown") move(at + 1);
    else if (e.key === "ArrowUp") move(at - 1);
    else if (e.key === "Home") move(0);
    else if (e.key === "End") move(-1);
    else if (e.key === "ArrowRight" && it?.items) openSub(items.indexOf(it), document.activeElement as HTMLElement);
    else if (e.key === "ArrowLeft" && onBack) onBack();
    else if (e.key === "Escape") (onBack ?? onClose)();
    else if (e.key === "Tab") onClose();
    else return;
    e.preventDefault();
  };

  const subItems = sub && items[sub.index]?.items;
  return createPortal(
    <>
      <div
        ref={ref}
        role="menu"
        className={`fixed ${layer.menu} min-w-40 panel rounded-control shadow-float py-1 text-sm`}
        style={pos}
        onClick={(e) => e.stopPropagation()}
        onKeyDown={onKeyDown}
      >
        {items.map((it, i) => (
          <button
            key={i}
            role="menuitem"
            data-index={i}
            aria-haspopup={it.items ? "menu" : undefined}
            aria-expanded={it.items ? sub?.index === i : undefined}
            disabled={it.disabled}
            onClick={(e) => pick(it, i, e.currentTarget)}
            onMouseEnter={(e) => (it.items ? openSub(i, e.currentTarget) : setSub(null))}
            className={
              "w-full text-left px-3 py-1.5 flex items-center gap-2 outline-none disabled:opacity-40 " +
              (it.danger ? "text-danger hover:bg-danger/10 focus:bg-danger/10" : "hover:bg-hover focus:bg-hover")
            }
          >
            {it.icon && <it.icon className="size-4 text-muted" aria-hidden />}
            <span className="flex-1">{it.label}</span>
            {it.shortcut && <Kbd>{it.shortcut}</Kbd>}
            {it.items && <CollapsedIcon className="size-3.5 text-muted" aria-hidden />}
          </button>
        ))}
      </div>
      {sub && subItems && (
        <Menu
          items={subItems}
          x={sub.x}
          y={sub.y}
          onClose={onClose}
          onBack={() => {
            const i = sub.index;
            setSub(null);
            ref.current!.querySelector<HTMLElement>(`[data-index="${i}"]`)?.focus();
          }}
        />
      )}
    </>,
    document.body
  );
}
