import { useEffect, useState } from "react";
import * as api from "../../api";
import { hostKind } from "../../api";
import { t } from "../../i18n";
import { useHosts } from "../../state/hosts";
import { Button } from "../../ui/Button";
import { useToast } from "../../ui/toast";
import { Group, Row } from "./Group";

/** Kinds that open a terminal, the ones a session log can follow. */
export const TERMINAL_KINDS = ["ssh", "telnet", "mosh", "tcp"];

/** Session logging (R69, KTD39): where logs go and which hosts log every
 *  session. Both are this computer's own settings. The folder changes only
 *  through a dialog Go opens. */
export function Logging() {
  const { hosts } = useHosts();
  const { push } = useToast();
  const [folder, setFolder] = useState("");
  const [logged, setLogged] = useState<Record<string, boolean>>({});
  const fail = (e: unknown) => push("error", api.errorInfo(e).message);
  const loadFolder = () => api.logFolder().then(setFolder, fail);
  useEffect(() => {
    loadFolder();
    api.settingsGetLocal().then((l) => setLogged(l.hostLogging ?? {}), fail);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const terminals = hosts.filter((h) => TERMINAL_KINDS.includes(hostKind(h)));

  return (
    <>
      <Group title={t("logging.folder")} description={t("logging.folderHint")}>
        <p className="text-sm font-mono allow-select break-all">{folder}</p>
        <div className="flex gap-2">
          <Button onClick={() => api.chooseLogFolder().then((dir) => void (dir && loadFolder()), fail)}>{t("logging.choose")}</Button>
          <Button variant="ghost" onClick={() => api.resetLogFolder().then(loadFolder, fail)}>
            {t("logging.default")}
          </Button>
        </div>
      </Group>
      <Group title={t("logging.hosts")} description={t("logging.hostsHint")}>
        {terminals.length === 0 ? (
          <p className="text-sm text-muted">{t("logging.noHosts")}</p>
        ) : (
          <ul className="flex flex-col">
            {terminals.map((h) => (
              <Row key={h.id}>
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={!!logged[h.id]}
                    onChange={(e) => {
                      const on = e.target.checked;
                      api.settingsSetLocal({ hostLogging: { [h.id]: on } }).then(() => setLogged((l) => ({ ...l, [h.id]: on })), fail);
                    }}
                  />
                  <span className="truncate">{h.alias}</span>
                  <span className="text-xs text-muted truncate">{h.hostname}</span>
                </label>
              </Row>
            ))}
          </ul>
        )}
      </Group>
    </>
  );
}
