import { useEffect, useState } from "react";
import { Browser } from "@wailsio/runtime";
import * as api from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { DownloadIcon, RefreshIcon } from "../../ui/icons";
import { Spinner } from "../../ui/kit";
import { usePrompt } from "../../ui/prompt";
import { Group } from "./Group";

/** The version and verified updates (R55). Nothing installs unless the user
 *  chooses Install; a deb or rpm install updates from the release page. */
export function About() {
  const { confirm } = usePrompt();
  const [status, setStatus] = useState<api.UpdateStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    api.updateStatus().then(setStatus).catch((e) => setError(api.errorInfo(e).message));
  }, []);
  const call = async (fn: () => Promise<api.UpdateStatus>) => {
    setBusy(true);
    setError("");
    try {
      setStatus(await fn());
    } catch (e) {
      setError(api.errorInfo(e).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Group title={t("app.name")}>
      <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm w-fit">
        <dt className="text-muted">{t("about.version")}</dt>
        <dd className="tabular-nums allow-select">{status?.current || "…"}</dd>
        <dt className="text-muted">{t("about.lastCheck")}</dt>
        <dd>{status?.lastCheck ? new Date(status.lastCheck).toLocaleString() : t("about.never")}</dd>
      </dl>
      {status && !status.signed && <p className="text-sm text-muted max-w-prose">{t("about.unsigned")}</p>}
      {status?.available ? (
        <div className="flex flex-wrap items-center gap-3">
          <p className="text-sm">{t("about.available", { version: status.available })}</p>
          {status.installed ? (
            <p className="text-sm text-success">{t("about.installed")}</p>
          ) : status.canInstall ? (
            <Button
              variant="primary"
              icon={DownloadIcon}
              disabled={busy}
              onClick={async () => {
                if (await confirm(t("about.installConfirm", { version: status.available }))) call(api.installUpdate);
              }}
            >
              {t("about.install")}
            </Button>
          ) : (
            status.pageUrl && <Button onClick={() => Browser.OpenURL(status.pageUrl)}>{t("about.releasePage")}</Button>
          )}
        </div>
      ) : (
        status?.lastCheck && <p className="text-sm text-muted">{t("about.upToDate")}</p>
      )}
      {(error || status?.error) && <p className="text-sm text-danger">{error || status?.error}</p>}
      <Button className="w-fit" icon={busy ? undefined : RefreshIcon} disabled={busy} onClick={() => call(() => api.checkForUpdate(false))}>
        {busy ? <Spinner /> : t("about.check")}
      </Button>
    </Group>
  );
}
