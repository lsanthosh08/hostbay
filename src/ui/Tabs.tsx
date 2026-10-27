import type { DragEvent, KeyboardEvent, ReactNode } from "react";
import { t } from "../i18n";
import { focusRing } from "./Button";
import { IconButton } from "./IconButton";
import { CloseIcon, type Icon } from "./icons";

export type TabItem<T extends string> = {
  id: T;
  label: ReactNode;
  icon?: Icon;
  /** Shows a close button on the tab. */
  onClose?: () => void;
  /** Tooltip. */
  title?: string;
};

/** A dragged tab's ID; a split pane takes a session tab dropped on it. */
export const DND_TAB = "text/tab";

/** A tab strip. The selected tab carries an accent underline; arrow keys,
 *  Home and End move between tabs. Panels are the caller's. */
export function Tabs<T extends string>({
  items,
  value,
  onChange,
  label,
  onReorder,
  className = "",
}: {
  items: TabItem<T>[];
  value: T;
  onChange: (id: T) => void;
  /** Accessible name of the strip. */
  label: string;
  /** Makes tabs draggable: drops `from` at `before`'s position. */
  onReorder?: (from: T, before: T) => void;
  className?: string;
}) {
  const onKeyDown = (e: KeyboardEvent) => {
    const at = items.findIndex((i) => i.id === value);
    const to =
      e.key === "ArrowRight" ? at + 1 : e.key === "ArrowLeft" ? at - 1 : e.key === "Home" ? 0 : e.key === "End" ? -1 : null;
    if (to === null || !items.length) return;
    e.preventDefault();
    const next = items[(to + items.length) % items.length];
    onChange(next.id);
    (e.currentTarget.querySelector(`[data-tab="${next.id}"]`) as HTMLElement | null)?.focus();
  };
  // A reorderable strip (the session tabs) scrolls instead of wrapping.
  return (
    <div role="tablist" aria-label={label} onKeyDown={onKeyDown} className={`flex items-center gap-1 ${onReorder ? "" : "flex-wrap"} ${className}`}>
      {items.map((it) => {
        const on = it.id === value;
        return (
          <div
            key={it.id}
            role="tab"
            aria-selected={on}
            data-tab={it.id}
            tabIndex={on ? 0 : -1}
            title={it.title}
            onClick={() => onChange(it.id)}
            // No state change while dragging: WKWebView cancels the drag.
            {...(onReorder && {
              draggable: true,
              onDragStart: (e: DragEvent) => e.dataTransfer.setData(DND_TAB, it.id),
              onDragOver: (e: DragEvent) => e.preventDefault(),
              onDrop: (e: DragEvent) => {
                e.preventDefault();
                const from = e.dataTransfer.getData(DND_TAB) as T;
                if (from && from !== it.id) onReorder(from, it.id);
              },
            })}
            className={
              `flex items-center gap-1.5 px-3 py-1.5 border-b-2 text-sm cursor-pointer whitespace-nowrap transition-colors ${focusRing} ` +
              (on ? "border-accent text-fg font-medium" : "border-transparent text-muted hover:text-fg")
            }
          >
            {it.icon && <it.icon className="size-4" aria-hidden />}
            {it.label}
            {it.onClose && (
              <IconButton
                size="sm"
                label={t("common.closeTab")}
                icon={CloseIcon}
                tabIndex={-1}
                className="-mr-1.5"
                onClick={(e) => {
                  e.stopPropagation();
                  it.onClose!();
                }}
              />
            )}
          </div>
        );
      })}
    </div>
  );
}
