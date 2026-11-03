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

/** What to ask for: reinstalling an out-of-date helper, installing a missing
 *  one that a connected VPN's sites wait on, or nothing. */
export function helperAsk(helper: api.HelperState, pac?: api.PacStatus): "reinstall" | "install" | null {
  if (helper === "reinstall") return "reinstall";
  return helper === "missing" && pac === "helper" ? "install" : null;
}

/** Asks to reinstall the network helper whenever it is out of date, or to
 *  install it once a connected VPN's sites need it: at start, when such a VPN
 *  connects, when the window gets focus again, and once a minute while it
 *  stays so. */
export function useHelperPrompt() {
  const { confirm } = usePrompt();
  const { push } = useToast();
  useEffect(() => {
    let asking = false;
    let declined = 0;
    let gone = false;
    const check = async () => {
      if (!askDue(asking, declined, Date.now())) return;
      let ask: ReturnType<typeof helperAsk>;
      try {
        const st = await api.helperStatus();
        ask = helperAsk(st.state, st.state === "missing" ? (await api.pacState()).state : undefined);
      } catch {
        return;
      }
      if (gone || !ask) return;
      // Held through the install too: the window regains focus when the
      // administrator prompt closes, and must not ask a second time.
      asking = true;
      try {
        const yes = await confirm(t(ask === "install" ? "helper.installPrompt" : "helper.reinstallPrompt"), {
          focusConfirm: true,
          confirmLabel: t(ask === "install" ? "routing.install" : "routing.reinstall"),
        });
        if (!yes) {
          declined = Date.now();
          return;
        }
        const after = await api.installHelper();
        const done = ask === "install" ? "helper.installed" : "helper.reinstalled";
        push(after.state === "ready" ? "success" : "info", t(after.state === "ready" ? done : "helper.reinstallPending"));
      } catch (e) {
        declined = Date.now();
        push("error", api.errorInfo(e).message);
      } finally {
        asking = false;
      }
    };
    void check();
    const timer = setInterval(() => void check(), EVERY);
    window.addEventListener("focus", check);
    const offPac = api.onPacState((s) => s.state === "helper" && void check());
    return () => {
      gone = true;
      clearInterval(timer);
      window.removeEventListener("focus", check);
      offPac.then((f) => f());
    };
  }, [confirm, push]);
}
