import { useEffect, useState } from "react";
import * as api from "../../api";
import { editable, keysFor, type Action } from "../../actions/registry";
import { holder, platform, refusal, setKey, type KeyInput, type KeyOverrides } from "../../actions/shortcuts";
import { t } from "../../i18n";
import { loadSettings, setKeys, settings } from "../../state/settings";
import { Button } from "../../ui/Button";
import { Kbd } from "../../ui/Kbd";
import { useToast } from "../../ui/toast";
import { Group, Row } from "./Group";
import { KeyRecorder } from "./KeyRecorder";

/** The keybinding editor (R66): every action, bindable on this platform
 *  only. A key another action holds is swapped on confirm; one the
 *  terminal needs is refused with the reason. */
export function Keyboard() {
  const { push } = useToast();
  const [keys, setLocal] = useState<KeyOverrides>(settings().keys ?? {});
  const [recording, setRecording] = useState<string | null>(null);
  // A recorded key some other action holds, waiting for Swap.
  const [clash, setClash] = useState<{ id: string; binding: string; other: Action } | null>(null);
  const [refused, setRefused] = useState<{ id: string; reason: string } | null>(null);
  useEffect(() => {
    loadSettings().then((s) => setLocal(s.keys ?? {}), () => {});
  }, []);

  const list = editable();
  const save = (next: KeyOverrides) =>
    setKeys(next).then(
      () => setLocal(next),
      (e) => push("error", api.errorInfo(e).message)
    );
  const recorded = (a: Action) => (binding: string, e: KeyInput) => {
    setRecording(null);
    const why = refusal(binding, a.shortcut, platform);
    if (why) return setRefused({ id: a.id, reason: t(why) });
    const other = holder(list, e, platform, a.id);
    if (other) return setClash({ id: a.id, binding, other });
    save(setKey(keys, a.id, platform, binding));
  };
  const swap = () => {
    if (!clash) return;
    // The other action takes this one's old keys, if it may have them.
    const old = list.find((a) => a.id === clash.id)?.shortcut?.[platform];
    const back = old && !refusal(old, clash.other.shortcut, platform) ? old : "";
    save(setKey(setKey(keys, clash.id, platform, clash.binding), clash.other.id, platform, back));
    setClash(null);
  };
  const changed = (id: string) => keys[id]?.[platform] != null;

  return (
    <Group title={t("settings.shortcuts")} description={t("settings.shortcutsHint")}>
      {list.some((a) => changed(a.id)) && (
        <div>
          <Button size="sm" onClick={() => save(Object.keys(keys).reduce((k, id) => setKey(k, id, platform, null), keys))}>
            {t("keys.resetAll")}
          </Button>
        </div>
      )}
      <ul className="flex flex-col max-w-2xl">
        {list.map((a) => (
          <Row
            key={a.id}
            actions={
              recording === a.id ? (
                <KeyRecorder onKey={recorded(a)} onCancel={() => setRecording(null)} />
              ) : clash?.id === a.id ? (
                <>
                  <Button size="sm" variant="primary" onClick={swap}>
                    {t("keys.swap")}
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => setClash(null)}>
                    {t("common.cancel")}
                  </Button>
                </>
              ) : (
                <>
                  <Kbd>{keysFor(a.id) ?? t("keys.none")}</Kbd>
                  <Button
                    size="sm"
                    onClick={() => {
                      setClash(null);
                      setRefused(null);
                      setRecording(a.id);
                    }}
                  >
                    {t("keys.change")}
                  </Button>
                  {changed(a.id) && (
                    <Button size="sm" variant="ghost" onClick={() => save(setKey(keys, a.id, platform, null))}>
                      {t("keys.reset")}
                    </Button>
                  )}
                </>
              )
            }
          >
            {t(a.label, a.vars)}
            {clash?.id === a.id && (
              <p className="text-xs text-muted" role="status">
                {t("keys.heldBy", { keys: keysFor(clash.other.id) ?? clash.binding, name: t(clash.other.label, clash.other.vars) })}
              </p>
            )}
            {refused?.id === a.id && (
              <p className="text-xs text-danger" role="alert">
                {refused.reason}
              </p>
            )}
          </Row>
        ))}
      </ul>
    </Group>
  );
}
