import { useEffect, useState } from "react";
import { dataFolder, dataFolderMove, errorInfo, type DataFolder } from "../../api";
import { setAlwaysOnTop, setZoom, windowPrefs } from "../../components/WindowControls";
import { language, LANGUAGES, t } from "../../i18n";
import { setLanguage } from "../../state/settings";
import { Button } from "../../ui/Button";
import { Select } from "../../ui/Select";
import { useAsyncToast } from "../../ui/toast";
import { Group } from "./Group";

/** Where the data lives, and the move to a folder Go asks for (R72). */
function DataFolderRow() {
  const run = useAsyncToast();
  const [folder, setFolder] = useState<DataFolder | null>(null);
  const load = () => run(dataFolder).then((f) => f && setFolder(f));
  useEffect(() => {
    load();
  }, []);
  return (
    <Group title={t("settings.dataFolder")} description={t("settings.dataFolderHint")}>
      <div className="flex items-center gap-3 max-w-xl">
        <code className="text-sm flex-1 min-w-0 truncate allow-select" title={folder?.path}>
          {folder?.path} {folder?.default && <span className="text-muted">{t("settings.dataFolderDefault")}</span>}
        </code>
        <Button size="sm" onClick={() => run(dataFolderMove).then(load)}>
          {t("settings.dataFolderMove")}
        </Button>
      </div>
      {folder?.pending && <p className="text-sm text-muted">{t("settings.dataFolderPending", { path: folder.pending })}</p>}
      {folder?.notice && (
        <p role="alert" className="text-sm text-danger allow-select break-words max-w-prose">
          {folder.notice}
        </p>
      )}
    </Group>
  );
}

/** Language, UI zoom, the window and the data folder. Themes are under
 *  Appearance. */
export function General() {
  const [lang, setLang] = useState<string>(language);
  const [langError, setLangError] = useState("");
  const [zoom, setZoomState] = useState(windowPrefs.zoom);
  const [onTop, setOnTop] = useState(windowPrefs.onTop);
  const zoomTo = (z: number) => {
    const next = Math.min(150, Math.max(70, z));
    setZoom(next);
    setZoomState(next);
  };
  return (
    <>
      <Group title={t("settings.language")} description={t("settings.languageHint")}>
        <Select
          className="w-48"
          aria-label={t("settings.language")}
          value={lang}
          onChange={(e) => {
            const next = e.target.value;
            setLang(next);
            setLangError("");
            setLanguage(next).catch((err) => setLangError(errorInfo(err).message));
          }}
        >
          {LANGUAGES.map((l) => (
            <option key={l.id} value={l.id} lang={l.id}>
              {l.name}
            </option>
          ))}
        </Select>
        {langError && <p className="text-sm text-danger">{langError}</p>}
      </Group>
      <Group title={t("window.zoom")} description={t("settings.zoomHint")}>
        <div className="flex items-center gap-3 max-w-sm">
          <input
            type="range"
            min={70}
            max={150}
            step={10}
            value={zoom}
            aria-label={t("window.zoom")}
            onChange={(e) => zoomTo(Number(e.target.value))}
            className="zoom-slider flex-1"
          />
          <span className="text-sm tabular-nums w-12 text-right">{zoom}%</span>
          <Button size="sm" variant="ghost" disabled={zoom === 100} onClick={() => zoomTo(100)}>
            {t("window.zoomReset")}
          </Button>
        </div>
      </Group>
      <Group title={t("settings.window")}>
        <label className="flex items-center gap-2 text-sm w-fit">
          <input
            type="checkbox"
            className="accent-accent size-4"
            checked={onTop}
            onChange={(e) => {
              setAlwaysOnTop(e.target.checked);
              setOnTop(e.target.checked);
            }}
          />
          {t("window.alwaysOnTop")}
        </label>
      </Group>
      <DataFolderRow />
    </>
  );
}
