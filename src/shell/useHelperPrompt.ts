import { useEffect } from "react";
import * as api from "../api";
import { t } from "../i18n";
import { usePrompt } from "../ui/prompt";
import { useToast } from "../ui/toast";

const EVERY = 60_000;
/** After a "Not now", wait this long before asking again (a restart asks at once). */
const COOLDOWN = 10 * 60_000;

/** Whether to ask now: not while a question is open, and not within COOLDOWN of a "Not now". */
export const askDue = (asking: boolean, declined: number, now: number) => !asking && now - declined >= COOLDOWN;

/** Asks to reinstall the network helper whenever it is out of date: at start,
 *  when the window gets focus again, and once a minute while it stays so. */
export function useHelperPrompt() {
  const { confirm } = usePrompt();
  const { push } = useToast();
  useEffect(() => {
    let asking = false;
    let declined = 0;
    let gone = false;
    const check = async () => {
      if (!askDue(asking, declined, Date.now())) return;
      let st: api.HelperStatus;
      try {
        st = await api.helperStatus();
      } catch {
        return;
      }
      if (gone || st.state !== "reinstall") return;
      asking = true;
      const yes = await confirm(t("helper.reinstallPrompt"), { focusConfirm: true, confirmLabel: t("routing.reinstall") });
      asking = false;
      if (!yes) {
        declined = Date.now();
        return;
      }
      try {
        const after = await api.installHelper();
        push(after.state === "ready" ? "success" : "info", t(after.state === "ready" ? "helper.reinstalled" : "helper.reinstallPending"));
      } catch (e) {
        push("error", api.errorInfo(e).message);
      }
    };
    void check();
    const timer = setInterval(() => void check(), EVERY);
    window.addEventListener("focus", check);
    return () => {
      gone = true;
      clearInterval(timer);
      window.removeEventListener("focus", check);
    };
  }, [confirm, push]);
}
