import { useEffect, useState } from "react";
import * as api from "../../api";
import { open } from "../../dialogs";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { IconButton } from "../../ui/IconButton";
import { DeleteIcon, ImportIcon } from "../../ui/icons";
import { usePrompt } from "../../ui/prompt";
import { useAsyncToast } from "../../ui/toast";
import { Group, Row } from "./Group";

/** Keys Hostbay keeps (imported, sealed) and the ones in ~/.ssh it can use. */
export function Keys() {
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const [saved, setSaved] = useState<string[]>([]);
  const [ssh, setSsh] = useState<string[]>([]);
  const reload = () => api.listIdentityKeys().then(setSaved).catch(() => {});
  useEffect(() => {
    reload();
    api.listSshKeys().then(setSsh).catch(() => {});
  }, []);

  const importKey = async () => {
    const p = await open({ filters: [{ name: "Keys", extensions: ["pem", "ppk", "key", ""] }] });
    if (typeof p !== "string") return;
    if (await run(() => api.saveIdentityKey(p), t("settings.keyImported"))) reload();
  };
  const remove = async (name: string) => {
    if (!(await confirm(t("settings.keyDeleteConfirm", { name })))) return;
    await run(() => api.deleteIdentityKey(name));
    reload();
  };

  return (
    <>
      <Group title={t("settings.importedKeys")} description={t("settings.importedKeysHint")}>
        <Button className="w-fit" icon={ImportIcon} onClick={importKey}>
          {t("hostForm.importKey")}
        </Button>
        {saved.length > 0 && (
          <ul className="flex flex-col max-w-xl">
            {saved.map((k) => (
              <Row key={k} actions={<IconButton label={t("settings.keyDelete")} icon={DeleteIcon} tone="danger" onClick={() => remove(k)} />}>
                <span className="font-mono allow-select">{k}</span>
              </Row>
            ))}
          </ul>
        )}
      </Group>
      <Group title={t("settings.sshKeys")} description={t("settings.sshKeysHint")}>
        {ssh.length === 0 ? (
          <p className="text-sm text-muted">{t("settings.sshKeysEmpty")}</p>
        ) : (
          <ul className="flex flex-col max-w-xl">
            {ssh.map((k) => (
              <Row key={k}>
                <span className="font-mono allow-select truncate block">{k}</span>
              </Row>
            ))}
          </ul>
        )}
      </Group>
    </>
  );
}
