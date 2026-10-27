import type { ReactNode, Ref } from "react";
import { t } from "../i18n";
import { Button } from "../ui/Button";
import { RefreshIcon } from "../ui/icons";
import { Spinner } from "../ui/kit";
import { StatusDot } from "../ui/StatusDot";

export type ScreenPhase = "opening" | "live" | "closed";

/** The frame VncTab and SpiceTab share: the toolbar with the keyboard hint,
 *  the area holding the screen, and its opening and closed overlays. Each
 *  tab wires its own protocol to the screen. */
export function RemoteScreen({
  phase,
  problem,
  onReconnect,
  tools,
  showKeys,
  focused,
  screen,
  areaRef,
  areaClassName = "flex-1 min-h-0 relative bg-ground",
  children,
}: {
  phase: ScreenPhase;
  problem: string;
  onReconnect: () => void;
  tools: ReactNode;
  showKeys: boolean;
  focused: boolean;
  screen: ReactNode;
  areaRef?: Ref<HTMLDivElement>;
  areaClassName?: string;
  children?: ReactNode;
}) {
  return (
    <div className="flex-1 min-h-0 flex flex-col">
      <div role="toolbar" aria-label={t("vnc.controls")} className="px-3 py-1.5 flex flex-wrap items-center gap-2 bg-surface border-b border-line">
        {tools}
        {showKeys && (
          <span aria-live="polite" className="ml-auto text-xs text-muted">
            {focused ? t("vnc.keyboardOn") : t("vnc.keyboardOff")}
          </span>
        )}
      </div>

      <div ref={areaRef} className={areaClassName}>
        {screen}
        {phase === "opening" && (
          <div className="absolute inset-0 flex items-center justify-center gap-2 text-sm">
            <Spinner /> {t("vnc.opening")}
          </div>
        )}
        {phase === "closed" && (
          <div className="absolute inset-0 flex items-center justify-center p-6 bg-ground">
            <section aria-live="polite" aria-label={t("vnc.closed")} className="w-full max-w-lg flex flex-col gap-4">
              <div className="flex items-center gap-2.5">
                <StatusDot status="error" />
                <h2 className="text-base font-semibold">{t("vnc.closed")}</h2>
              </div>
              {problem && <p className="text-sm allow-select break-words">{problem}</p>}
              <div className="flex flex-wrap gap-2">
                <Button variant="primary" icon={RefreshIcon} onClick={onReconnect}>
                  {t("vnc.reconnect")}
                </Button>
              </div>
            </section>
          </div>
        )}
      </div>

      {children}
    </div>
  );
}
