import { hostKind } from "../api";
import { t } from "../i18n";
import { sessionAddress, connLabel, neverConnected, useSessions, viewsFor } from "../state/sessions";
import { Button } from "../ui/Button";
import { IconButton } from "../ui/IconButton";
import { EditIcon, UnplugIcon, VncIcon } from "../ui/icons";
import { SegmentedControl } from "../ui/SegmentedControl";
import { StatusDot } from "../ui/StatusDot";
import { VIEW_INFO } from "./SessionLayer";

// Labels that give way to their icon when the toolbar is narrow; they stay
// in the accessibility tree, and the control's title shows them on hover.
const viewLabel = "sr-only @min-[54rem]:not-sr-only";
const disconnectLabel = "sr-only @min-[40rem]:not-sr-only";
const stateLabel = "sr-only @min-[30rem]:not-sr-only";

/** The active session's state and address, its view switcher (the views its
 *  host kind offers) and its actions. The address gives way first (the
 *  status bar shows it too), then the labels; nothing wraps or overflows. */
export function SessionToolbar({ onEditHost }: { onEditHost: () => void }) {
  const { active, views, setView, disconnect, openX11Desktop } = useSessions();
  if (!active) return null;
  const options = viewsFor(active);
  return (
    <div className="@container h-11 shrink-0 flex items-center gap-3 px-3 border-b border-line">
      <span className="flex items-center gap-1.5 text-sm shrink-0 whitespace-nowrap">
        <StatusDot status={active.status} />
        <span className={stateLabel}>{connLabel(active)}</span>
      </span>
      <span className="flex-1 text-sm text-muted font-mono truncate min-w-0 allow-select">{sessionAddress(active)}</span>
      {options.length > 1 && !active.dialing && (
        <SegmentedControl
          label={t("view.label")}
          value={views[active.sessionId]}
          onChange={(v) => setView(active.sessionId, v)}
          options={options.map((v) => ({ value: v, icon: VIEW_INFO[v].icon, title: VIEW_INFO[v].label, label: <span className={viewLabel}>{VIEW_INFO[v].label}</span> }))}
          className="shrink-0"
        />
      )}
      <div className="flex items-center gap-1 shrink-0">
        {/* An SSH host's workspace opens its X11 desktop in a tab of its own. */}
        {hostKind(active.host) === "ssh" && !active.x11 && !active.term && !active.files && !active.engine && (
          <IconButton label={t("x11.openDesktop")} icon={VncIcon} onClick={() => openX11Desktop(active.host)} />
        )}
        {/* A local shell's, serial port's or the Files tab's stand-in host is not saved. */}
        {!active.term && !active.files && !active.engine && <IconButton label={t("action.editHost")} icon={EditIcon} onClick={onEditHost} />}
        {/* A tab that never connected has nothing to disconnect; its tab closes it. */}
        {!neverConnected(active) && (
          <Button
            variant="ghost"
            size="sm"
            icon={UnplugIcon}
            title={t("action.disconnect")}
            className="ml-1 whitespace-nowrap"
            onClick={() => disconnect(active.sessionId)}
          >
            <span className={disconnectLabel}>{t("action.disconnect")}</span>
          </Button>
        )}
      </div>
    </div>
  );
}
