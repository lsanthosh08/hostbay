import { useEffect } from "react";
import { Browser } from "@wailsio/runtime";
import * as api from "../api";
import { t } from "../i18n";
import { usePrompt } from "../ui/prompt";
import { useToast } from "../ui/toast";

const DISMISSED_VERSION = "updateDismissed";

export const offerUpdate = (s: api.UpdateStatus, dismissed: string | null) =>
  !!s.available && !s.installed && s.available !== dismissed;

/** Offers a verified update: at start for one the last check found (the
 *  startup check can finish before the window loads), and whenever the
 *  background check finds one. Nothing installs unless the user chooses
 *  Install; a deb or rpm install opens the release page instead. */
export function useUpdatePrompt() {
  const { confirm } = usePrompt();
  const { push } = useToast();
  useEffect(() => {
    let asking = false;
    let gone = false;
    const offer = async (s: api.UpdateStatus) => {
      if (asking || gone) return;
      asking = true;
      try {
        if (!offerUpdate(s, localStorage.getItem(DISMISSED_VERSION))) return;
        const version = s.available;
        const yes = await confirm(t(s.canInstall ? "about.installConfirm" : "about.available", { version }), {
          focusConfirm: true,
          confirmLabel: t(s.canInstall ? "about.install" : "about.releasePage"),
          cancelLabel: t("about.later"),
        });
        if (!yes) {
          localStorage.setItem(DISMISSED_VERSION, version);
          return;
        }
        if (!s.canInstall) {
          localStorage.setItem(DISMISSED_VERSION, version);
          Browser.OpenURL(s.pageUrl);
          return;
        }
        push("info", t("about.downloading", { version }));
        const after = await api.installUpdate();
        if (after.installed) push("success", t("about.installed"));
      } catch (e) {
        push("error", api.errorInfo(e).message);
      } finally {
        asking = false;
      }
    };
    api.updateStatus().then(offer, () => {});
    const off = api.onUpdateAvailable(offer);
    return () => {
      gone = true;
      off.then((f) => f());
    };
  }, [confirm, push]);
}
