import { useEffect, useState } from "react";
import * as api from "../api";
import { t } from "../i18n";
import { Button } from "../ui/Button";
import { Dialog } from "../ui/Dialog";
import { Field } from "../ui/Field";
import { IconButton } from "../ui/IconButton";
import { RefreshIcon } from "../ui/icons";
import { Select } from "../ui/Select";

const BAUDS = [300, 1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200, 230400, 460800, 921600];

/** Picks a serial port and its line settings (R20). */
export function SerialDialog({
  onOpen,
  onCancel,
}: {
  onOpen: (s: api.SerialSettings) => void;
  onCancel: () => void;
}) {
  const [ports, setPorts] = useState<string[] | null>(null);
  const [error, setError] = useState("");
  const [s, setS] = useState<api.SerialSettings>({
    port: "",
    baud: 9600,
    dataBits: 8,
    parity: "none",
    stopBits: "1",
    localEcho: false,
  });
  const set = (p: Partial<api.SerialSettings>) => setS((v) => ({ ...v, ...p }));

  const load = () =>
    api
      .listSerialPorts()
      .then((p) => {
        setPorts(p);
        setError("");
        setS((v) => (p.includes(v.port) ? v : { ...v, port: p[0] ?? "" }));
      })
      .catch((e) => setError(api.errorInfo(e).message));
  useEffect(() => {
    load();
  }, []);

  return (
    <Dialog title={t("serial.title")} size="sm" onClose={onCancel}>
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (s.port) onOpen(s);
        }}
      >
        <Field label={t("serial.port")}>
          <div className="flex gap-2">
            <Select className="w-full" value={s.port} onChange={(e) => set({ port: e.target.value })}>
              {ports?.length === 0 && <option value="">{t("serial.noPorts")}</option>}
              {ports?.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </Select>
            <IconButton label={t("serial.rescan")} icon={RefreshIcon} className="self-center" onClick={load} />
          </div>
        </Field>
        <Field label={t("serial.baud")}>
          <Select className="w-full" value={s.baud} onChange={(e) => set({ baud: Number(e.target.value) })}>
            {BAUDS.map((b) => (
              <option key={b} value={b}>
                {b}
              </option>
            ))}
          </Select>
        </Field>
        <div className="grid grid-cols-3 gap-2">
          <Field label={t("serial.dataBits")}>
            <Select className="w-full" value={s.dataBits} onChange={(e) => set({ dataBits: Number(e.target.value) })}>
              {[8, 7, 6, 5].map((b) => (
                <option key={b} value={b}>
                  {b}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={t("serial.parity")}>
            <Select
              className="w-full"
              value={s.parity}
              onChange={(e) => set({ parity: e.target.value as api.SerialSettings["parity"] })}
            >
              {["none", "odd", "even", "mark", "space"].map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={t("serial.stopBits")}>
            <Select
              className="w-full"
              value={s.stopBits}
              onChange={(e) => set({ stopBits: e.target.value as api.SerialSettings["stopBits"] })}
            >
              {["1", "1.5", "2"].map((b) => (
                <option key={b} value={b}>
                  {b}
                </option>
              ))}
            </Select>
          </Field>
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={s.localEcho} onChange={(e) => set({ localEcho: e.target.checked })} />
          {t("serial.localEcho")}
        </label>
        {error && <p className="text-xs text-danger">{error}</p>}
        <div className="flex gap-2 justify-end">
          <Button variant="ghost" type="button" onClick={onCancel}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" type="submit" disabled={!s.port}>
            {t("serial.open")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
