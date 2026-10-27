import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { t } from "../i18n";
import { IconButton } from "./IconButton";
import { CloseIcon } from "./icons";
import { layer } from "./layers";

const SIZES = {
  sm: "max-w-sm",
  md: "max-w-md",
  lg: "max-w-xl",
  xl: "max-w-2xl",
  full: "max-w-6xl h-[88vh]",
};

// A drawer docks to the right edge at full height (the host editor).
const PLACE = {
  center: ["items-center justify-center p-4", "rounded-card max-h-[90vh]"],
  drawer: ["items-stretch justify-end", "h-full border-l border-line"],
};

const FOCUSABLE =
  'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

// Open dialogs, newest last. Only the top one answers Escape and traps Tab,
// so a prompt opened over a form closes alone.
const stack: HTMLElement[] = [];

/**
 * A modal dialog in a portal: focus moves in and is trapped, Escape and a
 * backdrop click close it while `dismissible`, and focus returns on close.
 * `placement="drawer"` docks it to the right edge at full height.
 * Pass `dismissible={!dirty}` for a form so a stray key or click keeps edits;
 * the close button and the form's own Cancel still close it.
 */
export function Dialog({
  title,
  label,
  onClose,
  dismissible = true,
  size = "md",
  placement = "center",
  footer,
  bodyClassName = "p-4 overflow-auto",
  children,
}: {
  /** Heading with a close button. Without one, give `label` instead. */
  title?: ReactNode;
  label?: string;
  onClose: () => void;
  dismissible?: boolean;
  size?: keyof typeof SIZES;
  placement?: keyof typeof PLACE;
  /** Actions row, right-aligned under the body. */
  footer?: ReactNode;
  bodyClassName?: string;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const body = useRef<HTMLDivElement>(null);
  const titleId = useId();
  // The key listener is added once; it reads the latest props from here.
  const live = useRef({ onClose, dismissible });
  live.current = { onClose, dismissible };
  const downOnBackdrop = useRef(false);
  // Read during render: autoFocus inside moves focus before effects run.
  const [before] = useState(() => document.activeElement as HTMLElement | null);

  useEffect(() => {
    const el = ref.current!;
    stack.push(el);
    // autoFocus inside has already run; otherwise focus the body's first
    // control rather than the header's close button.
    if (!el.contains(document.activeElement))
      (body.current!.querySelector<HTMLElement>(FOCUSABLE) ?? el.querySelector<HTMLElement>(FOCUSABLE) ?? el).focus();
    const onKey = (e: KeyboardEvent) => {
      if (stack[stack.length - 1] !== el || e.isComposing) return;
      if (e.key === "Escape") {
        e.preventDefault();
        if (live.current.dismissible) live.current.onClose();
      } else if (e.key === "Tab") {
        const items = [...el.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(
          (n) => n.tabIndex >= 0 && n.getClientRects().length
        );
        if (!items.length) return e.preventDefault();
        const first = items[0];
        const last = items[items.length - 1];
        const inside = el.contains(document.activeElement);
        if (e.shiftKey && (!inside || document.activeElement === first)) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && (!inside || document.activeElement === last)) {
          e.preventDefault();
          first.focus();
        }
      }
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("keydown", onKey);
      stack.splice(stack.indexOf(el), 1);
      before?.focus();
    };
  }, [before]);

  return createPortal(
    <div
      className={`fixed inset-0 ${layer.dialog} bg-scrim flex ${PLACE[placement][0]}`}
      // Close only on a click that both starts and ends on the backdrop, so a
      // text selection dragged out of a field does not dismiss the dialog.
      onMouseDown={(e) => (downOnBackdrop.current = e.target === e.currentTarget)}
      onClick={(e) => {
        if (e.target === e.currentTarget && downOnBackdrop.current && dismissible) onClose();
      }}
    >
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-labelledby={title ? titleId : undefined}
        aria-label={title ? undefined : label}
        data-drawer={placement === "drawer" || undefined}
        tabIndex={-1}
        className={`panel shadow-float w-full ${SIZES[size]} ${PLACE[placement][1]} flex flex-col outline-none`}
      >
        {title && (
          <div className="px-4 py-3 border-b border-line flex items-center gap-2 shrink-0">
            <h2 id={titleId} className="font-semibold text-sm flex-1 min-w-0 truncate">
              {title}
            </h2>
            <IconButton label={t("common.close")} icon={CloseIcon} onClick={onClose} />
          </div>
        )}
        <div ref={body} className={`flex-1 min-h-0 ${bodyClassName}`}>
          {children}
        </div>
        {footer && <div className="px-4 py-3 border-t border-line flex justify-end gap-2 shrink-0">{footer}</div>}
      </div>
    </div>,
    document.body
  );
}
