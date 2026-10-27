import { createContext, useContext, useId, type InputHTMLAttributes, type ReactNode } from "react";

export const controlCls =
  "rounded-control border border-control bg-field text-fg text-sm placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus disabled:opacity-50";

// The enclosing Field's label target and hint or error, for its control.
const FieldCtx = createContext<{ id: string; describedBy?: string } | null>(null);

/** The id and description a control takes from its Field, so the label
 *  names it. A control with its own id or aria-label keeps its own name
 *  (a second control in one Field, like the key path under the key picker). */
export function useFieldControl(props: { id?: string; "aria-label"?: string; "aria-labelledby"?: string }) {
  const f = useContext(FieldCtx);
  if (!f || props.id || props["aria-label"] || props["aria-labelledby"]) return {};
  return { id: f.id, "aria-describedby": f.describedBy };
}

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...useFieldControl(props)} {...props} className={`h-9 px-3 ${controlCls} ${props.className || ""}`} />;
}

/** A label over its control, with an optional hint or inline error. The
 *  label names the Input or Select inside (useFieldControl). */
export function Field({
  label,
  hint,
  error,
  className = "",
  children,
}: {
  label: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
  className?: string;
  children: ReactNode;
}) {
  const id = useId();
  const noteId = `${id}-note`;
  const note = error || hint;
  return (
    <div className={className}>
      <label htmlFor={id} className="block text-xs text-muted mb-1">
        {label}
      </label>
      <FieldCtx.Provider value={{ id, describedBy: note ? noteId : undefined }}>{children}</FieldCtx.Provider>
      {note && (
        <p id={noteId} className={`mt-1 text-xs ${error ? "text-danger" : "text-muted"}`}>
          {note}
        </p>
      )}
    </div>
  );
}
