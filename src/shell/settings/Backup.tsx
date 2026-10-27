import { useEffect, useId, useState } from "react";
import * as api from "../../api";
import type { Host } from "../../api";
import { useHostManager } from "../../components/HostManager";
import { open, save } from "../../dialogs";
import { t } from "../../i18n";
import { importFrom, type ImportReport, type ImportTool } from "../../importApi";
import { useHosts } from "../../state/hosts";
import { Button } from "../../ui/Button";
import { Field, Input } from "../../ui/Field";
import { Select } from "../../ui/Select";
import { ArchiveIcon, DownloadIcon, ImportIcon } from "../../ui/icons";
import { usePrompt } from "../../ui/prompt";
import { useAsyncToast } from "../../ui/toast";
import { Group, Row } from "./Group";
import { IMPORT_TOOLS } from "./importers";

/**
 * Encrypted (the default, R16) or plain, then where to save. An encrypted
 * file holds passwords and keys under the passphrase; a plain one holds no
 * secrets. Both need OS verification in the backend. `name` is the default
 * file name without its extension.
 */
export function BackupForm({
  name,
  onExport,
}: {
  name: string;
  onExport: (path: string, passphrase: string) => Promise<unknown>;
}) {
  const id = useId();
  const [encrypted, setEncrypted] = useState(true);
  const [pass, setPass] = useState("");
  const [again, setAgain] = useState("");
  const mismatch = encrypted && again !== "" && again !== pass;
  const ready = !encrypted || (pass !== "" && pass === again);
  const option = (value: boolean, label: string, hint: string) => (
    <label className="flex gap-2.5 text-sm cursor-pointer">
      <input
        type="radio"
        name={`${id}-format`}
        className="accent-accent size-4 mt-0.5 shrink-0"
        checked={encrypted === value}
        onChange={() => setEncrypted(value)}
      />
      <span>
        <span className="font-medium">{label}</span>
        <span className="block text-muted">{hint}</span>
      </span>
    </label>
  );
  return (
    <form
      className="flex flex-col gap-4 max-w-md"
      onSubmit={async (e) => {
        e.preventDefault();
        const path = await save({
          defaultPath: `${name}.${encrypted ? "hostbay" : "json"}`,
          filters: [encrypted ? { name: "Hostbay backup", extensions: ["hostbay"] } : { name: "Plain backup", extensions: ["json"] }],
        });
        if (!path) return;
        await onExport(path, encrypted ? pass : "");
        setPass("");
        setAgain("");
      }}
    >
      <fieldset className="flex flex-col gap-3">
        <legend className="sr-only">{t("backup.format")}</legend>
        {option(true, t("backup.encrypted"), t("backup.encryptedHint"))}
        {option(false, t("backup.plain"), t("backup.plainHint"))}
      </fieldset>
      {encrypted && (
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Field label={t("backup.passphrase")}>
            <Input type="password" className="w-full" autoComplete="new-password" value={pass} onChange={(e) => setPass(e.target.value)} />
          </Field>
          <Field label={t("backup.passphraseAgain")} error={mismatch ? t("backup.mismatch") : undefined}>
            <Input type="password" className="w-full" autoComplete="new-password" value={again} onChange={(e) => setAgain(e.target.value)} />
          </Field>
        </div>
      )}
      <Button type="submit" variant="primary" icon={DownloadIcon} className="w-fit" disabled={!ready}>
        {t("backup.save")}
      </Button>
    </form>
  );
}

/**
 * Import another app's export (R54): pick the app, pick its file, give a
 * password only where that app encrypts saved passwords, then read what was
 * imported, what was skipped and why, and what needs a look.
 */
function ImportFromApp() {
  const { reloadAll } = useHosts();
  const run = useAsyncToast();
  const [tool, setTool] = useState<ImportTool>("mobaxterm");
  const [path, setPath] = useState("");
  const [pass, setPass] = useState("");
  const [busy, setBusy] = useState(false);
  const [report, setReport] = useState<{ name: string; r: ImportReport } | null>(null);
  const info = IMPORT_TOOLS.find((x) => x.id === tool)!;

  const choose = async () => {
    const p = await open({ filters: [{ name: info.name, extensions: info.extensions }, { name: "All files", extensions: ["*"] }] });
    if (typeof p === "string") setPath(p);
  };
  const skipped = report?.r.skipped ?? [];
  const notes = report?.r.notes ?? [];

  return (
    <div className="flex flex-col gap-4 max-w-xl">
      <form
        className="flex flex-col gap-3"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          const r = await run(() => importFrom(tool, path.trim(), info.password ? pass : ""));
          setBusy(false);
          if (r === undefined) return;
          setPass("");
          setReport({ name: info.name, r });
          reloadAll();
        }}
      >
        <Field label={t("import.tool")}>
          <Select
            className="w-full sm:w-64"
            value={tool}
            onChange={(e) => {
              setTool(e.target.value as ImportTool);
              setPass("");
              setReport(null);
            }}
          >
            {IMPORT_TOOLS.map((x) => (
              <option key={x.id} value={x.id}>
                {x.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label={t(info.path ?? "import.path")} hint={t(info.file)}>
          <div className="flex gap-2">
            <Input className="flex-1 min-w-0" value={path} spellCheck={false} onChange={(e) => setPath(e.target.value)} />
            <Button type="button" onClick={choose}>
              {t("import.choose")}
            </Button>
          </div>
        </Field>
        {info.password && (
          <Field label={t(info.password)} hint={t("import.passwordHint")}>
            <Input type="password" className="w-full sm:w-64" autoComplete="off" value={pass} onChange={(e) => setPass(e.target.value)} />
          </Field>
        )}
        <Button type="submit" variant="primary" icon={ImportIcon} className="w-fit" disabled={!path.trim() || busy}>
          {t("import.run")}
        </Button>
      </form>
      {report && (
        <div role="status" className="flex flex-col gap-3 text-sm">
          <p>{t("import.summary", { hosts: report.r.hosts, folders: report.r.folders, tool: report.name })}</p>
          {report.r.passwordsNeeded > 0 && <p className="text-muted">{t("import.passwordsNeeded", { count: report.r.passwordsNeeded })}</p>}
          {skipped.length > 0 && (
            <div>
              <h4 className="text-xs font-semibold text-muted mb-1">{t("import.skipped", { count: skipped.length })}</h4>
              <ul className="flex flex-col">
                {skipped.map((s, i) => (
                  <Row key={i}>
                    <span className="font-medium break-words">{s.file}</span> <span className="text-muted break-words">{s.reason}</span>
                  </Row>
                ))}
              </ul>
            </div>
          )}
          {notes.length > 0 && (
            <div>
              <h4 className="text-xs font-semibold text-muted mb-1">{t("import.notes", { count: notes.length })}</h4>
              <ul className="flex flex-col">
                {notes.map((n, i) => (
                  <Row key={i}>
                    <span className="break-words">{n}</span>
                  </Row>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/** Backup, restore, imports and the Trash. */
export function Backup() {
  const manager = useHostManager();
  const { reloadAll } = useHosts();
  const run = useAsyncToast();
  const { confirm } = usePrompt();
  const [trash, setTrash] = useState<Host[]>([]);
  const reloadTrash = () => api.listTrash().then(setTrash).catch(() => {});
  useEffect(() => {
    reloadTrash();
  }, []);

  const restore = async (h: Host) => {
    await run(() => api.restoreHost(h.id), t("trash.restored", { name: h.alias }));
    reloadTrash();
    reloadAll();
  };
  const purge = async (h: Host) => {
    if (!(await confirm(t("trash.purgeConfirm", { name: h.alias })))) return;
    await run(() => api.purgeHost(h.id), t("trash.purged"));
    reloadTrash();
  };

  return (
    <>
      <Group title={t("backup.title")} description={t("backup.hint")}>
        <BackupForm name="hostbay-backup" onExport={(path, pass) => run(() => api.exportStore(path, pass), t("backup.saved"))} />
      </Group>
      <Group title={t("backup.restoreTitle")} description={t("backup.restoreHint")}>
        <div className="flex flex-wrap gap-2">
          <Button icon={ArchiveIcon} onClick={manager.restore}>
            {t("action.restore")}
          </Button>
          <Button variant="ghost" icon={ImportIcon} onClick={manager.importSshConfig}>
            {t("action.importSshConfig")}
          </Button>
          <Button variant="ghost" icon={ImportIcon} onClick={manager.importSshManager}>
            {t("action.importSshManager")}
          </Button>
        </div>
      </Group>
      <Group title={t("import.title")} description={t("import.hint")}>
        <ImportFromApp />
      </Group>
      <Group title={t("trash.title", { count: trash.length })} description={t("trash.hint")}>
        {trash.length === 0 ? (
          <p className="text-sm text-muted">{t("trash.empty")}</p>
        ) : (
          <ul className="flex flex-col max-w-xl">
            {trash.map((h) => (
              <Row
                key={h.id}
                actions={
                  <>
                    <Button size="sm" variant="ghost" onClick={() => restore(h)}>
                      {t("trash.restore")}
                    </Button>
                    <Button size="sm" variant="danger" onClick={() => purge(h)}>
                      {t("trash.purge")}
                    </Button>
                  </>
                }
              >
                <span className="truncate block">
                  {h.alias} <span className="text-xs text-muted">{h.user}@{h.hostname}</span>
                </span>
              </Row>
            ))}
          </ul>
        )}
      </Group>
    </>
  );
}
