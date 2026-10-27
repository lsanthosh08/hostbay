import { useEffect, useState } from "react";
import * as api from "../../api";
import { RemoveKeyForm } from "../../components/TrustDialog";
import { useHostManager } from "../../components/HostManager";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { EmptyState } from "../../ui/EmptyState";
import { KeyIcon } from "../../ui/icons";
import { Group, Row } from "./Group";

/** The master password and the known host keys. A key is removed only after
 *  typing its host name and OS verification, as after a refusal (R13). */
export function Security() {
  const manager = useHostManager();
  const [known, setKnown] = useState<api.KnownHost[] | null>(null);
  const [error, setError] = useState("");
  // The entry whose removal form is open, as "host:port".
  const [removing, setRemoving] = useState("");
  const reload = () =>
    api
      .listKnownHosts()
      .then(setKnown)
      .catch((e) => setError(api.errorInfo(e).message));
  useEffect(() => {
    reload();
  }, []);

  return (
    <>
      <Group title={t("settings.masterPassword")} description={t("settings.masterPasswordHint")}>
        {manager.needsMasterPassword ? (
          <Button className="w-fit" icon={KeyIcon} onClick={manager.setMasterPassword}>
            {t("action.setMasterPassword")}
          </Button>
        ) : (
          <p className="text-sm">{t("settings.verifyByOs")}</p>
        )}
      </Group>
      <Group title={t("settings.knownHosts")} description={t("settings.knownHostsHint")}>
        {error && <p className="text-sm text-danger">{error}</p>}
        {known?.length === 0 && <EmptyState icon={KeyIcon} title={t("settings.knownHostsEmpty")} />}
        <ul className="flex flex-col">
          {known?.map((k, i) => {
            const id = `${k.host}:${k.port}`;
            return (
              <Row
                key={`${id}-${i}`}
                actions={
                  !k.hashed &&
                  removing !== id && (
                    <Button size="sm" variant="ghost" onClick={() => setRemoving(id)}>
                      {t("trust.removeStart")}
                    </Button>
                  )
                }
              >
                <div className="truncate">
                  {k.hashed ? (
                    <span className="text-muted">{t("settings.hashedHost")}</span>
                  ) : (
                    <>
                      {k.host}
                      {k.port !== 22 && <span className="text-muted">:{k.port}</span>}
                    </>
                  )}
                </div>
                <div className="text-xs text-muted font-mono truncate allow-select" title={k.fingerprint}>
                  {k.keyType} {k.fingerprint}
                </div>
                {removing === id && (
                  <div className="mt-3">
                    <RemoveKeyForm
                      host={k.host}
                      onCancel={() => setRemoving("")}
                      onRemove={async (typed) => {
                        await api.removeKnownHost(k.host, k.port, typed);
                        setRemoving("");
                        reload();
                      }}
                    />
                  </div>
                )}
              </Row>
            );
          })}
        </ul>
      </Group>
    </>
  );
}
