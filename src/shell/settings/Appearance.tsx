import { useEffect, useRef, useState } from "react";
import { backgroundClear, backgroundPick, errorInfo } from "../../api";
import { t, type MessageKey } from "../../i18n";
import {
  backgroundImage,
  loadBackground,
  loadSettings,
  onSettingsChange,
  setAppearance,
  settings,
  showBackground,
  type Appearance as Change,
} from "../../state/settings";
import { parseItermColors } from "../../theme/itermcolors";
import { terminalPalette, xtermTheme, type CustomTheme } from "../../theme/terminal";
import { longHex, PALETTE_KEYS, TERMINAL_THEMES, type Palette } from "../../theme/terminalThemes";
import { Button } from "../../ui/Button";
import { Field, Input } from "../../ui/Field";
import { SegmentedControl } from "../../ui/SegmentedControl";
import { Select } from "../../ui/Select";
import { useTheme, type Theme } from "../../ui/theme";
import { Group } from "./Group";

const UI_THEMES: { value: Theme; label: string }[] = [
  { value: "dark", label: t("settings.theme.dark") },
  { value: "deep-blue", label: t("settings.theme.deepBlue") },
  { value: "light", label: t("settings.theme.light") },
];

/** The UI theme, the terminal theme, custom terminal themes and the
 *  terminal background image (R67, KTD41). */
export function Appearance() {
  const { theme, setTheme } = useTheme();
  const [s, setS] = useState(settings);
  const [image, setImage] = useState(backgroundImage);
  const [note, setNote] = useState("");
  const [error, setError] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    loadSettings().then(() => setS({ ...settings() }), () => {});
    loadBackground();
    return onSettingsChange(() => {
      setS({ ...settings() });
      setImage(backgroundImage());
    });
  }, []);

  const custom = s.customThemes ?? [];
  const selected = custom.find((c) => c.id === s.terminalTheme);
  const save = (change: Change) => {
    setError("");
    setAppearance(change).catch((e) => setError(errorInfo(e).message));
  };
  // A new custom theme starts from the palette in use.
  const addCustom = (name: string, colors: Palette) => {
    const type = TERMINAL_THEMES.find((th) => th.id === s.terminalTheme)?.type ?? selected?.type ?? (theme === "light" ? "light" : "dark");
    const id = `custom-${Date.now().toString(36)}`;
    save({ customThemes: [...custom, { id, name, type, colors }], terminalTheme: id });
  };
  // With "Match UI theme" that is the UI theme's terminal tokens.
  const current = (): Palette => {
    const chosen = terminalPalette(s.terminalTheme, custom);
    if (chosen) return chosen;
    const x = xtermTheme() as Record<string, string>;
    return Object.fromEntries(PALETTE_KEYS.map((k) => [k, longHex(k === "selection" ? x.selectionBackground : x[k])])) as Palette;
  };
  const updateCustom = (id: string, change: Partial<CustomTheme>) =>
    save({ customThemes: custom.map((c) => (c.id === id ? { ...c, ...change } : c)) });

  const importFile = async (file: File) => {
    setNote("");
    try {
      const { colors, missing } = parseItermColors(await file.text(), current());
      addCustom(file.name.replace(/\.itermcolors$/i, "").slice(0, 100) || t("appearance.custom.defaultName", { n: custom.length + 1 }), colors);
      const names = missing.map((k) => t(("palette." + k) as MessageKey));
      if (names.length) setNote(t("appearance.custom.missing", { colors: names.join(", ") }));
    } catch {
      setError(t("appearance.custom.bad"));
    }
  };

  const pickImage = () => {
    setError("");
    backgroundPick().then(
      (url) => url && showBackground(url),
      (e) => setError(errorInfo(e).message)
    );
  };
  const removeImage = () => {
    setError("");
    backgroundClear().then(
      () => showBackground(""),
      (e) => setError(errorInfo(e).message)
    );
  };

  return (
    <>
      <Group title={t("settings.theme")}>
        <SegmentedControl label={t("settings.theme")} value={theme} onChange={setTheme} options={UI_THEMES} />
      </Group>

      <Group title={t("appearance.termTheme")} description={t("appearance.termThemeHint")}>
        <div className="flex flex-wrap items-center gap-2">
          <Select
            className="w-64"
            aria-label={t("appearance.termTheme")}
            value={s.terminalTheme ?? ""}
            onChange={(e) => save({ terminalTheme: e.target.value })}
          >
            <option value="">{t("appearance.termTheme.match")}</option>
            {(["dark", "light"] as const).map((type) => (
              <optgroup key={type} label={t(type === "dark" ? "settings.theme.dark" : "settings.theme.light")}>
                {TERMINAL_THEMES.filter((th) => th.type === type).map((th) => (
                  <option key={th.id} value={th.id}>
                    {th.label}
                  </option>
                ))}
              </optgroup>
            ))}
            {custom.length > 0 && (
              <optgroup label={t("appearance.termTheme.custom")}>
                {custom.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name}
                  </option>
                ))}
              </optgroup>
            )}
          </Select>
          <Button variant="ghost" onClick={() => addCustom(t("appearance.custom.defaultName", { n: custom.length + 1 }), { ...current() })}>
            {t("appearance.custom.new")}
          </Button>
          <Button variant="ghost" onClick={() => fileRef.current?.click()}>
            {t("appearance.custom.import")}
          </Button>
          <input
            ref={fileRef}
            type="file"
            accept=".itermcolors"
            className="sr-only"
            tabIndex={-1}
            aria-hidden
            onChange={(e) => {
              const f = e.target.files?.[0];
              e.target.value = "";
              if (f) importFile(f);
            }}
          />
        </div>
        {note && <p className="text-sm text-muted">{note}</p>}
        {selected && (
          <CustomEditor
            key={selected.id}
            theme={selected}
            onChange={(c) => updateCustom(selected.id, c)}
            onDelete={() => save({ customThemes: custom.filter((c) => c.id !== selected.id), terminalTheme: "" })}
          />
        )}
      </Group>

      <Group title={t("appearance.background")} description={t("appearance.backgroundHint")}>
        <div className="flex items-center gap-2">
          <Button onClick={pickImage}>{t("appearance.background.choose")}</Button>
          {image && (
            <Button variant="ghost" onClick={removeImage}>
              {t("appearance.background.remove")}
            </Button>
          )}
        </div>
        {image && (
          <div className="flex items-center gap-3 max-w-sm">
            <img src={image} alt="" className="h-12 w-20 object-cover rounded-control border border-line" />
            <input
              type="range"
              min={5}
              max={100}
              step={5}
              value={s.backgroundOpacity || 30}
              aria-label={t("appearance.background.opacity")}
              onChange={(e) => save({ backgroundOpacity: Number(e.target.value) })}
              className="zoom-slider flex-1"
            />
            <span className="text-sm tabular-nums w-12 text-right">{s.backgroundOpacity || 30}%</span>
          </div>
        )}
      </Group>
      {error && <p className="text-sm text-danger">{error}</p>}
    </>
  );
}

/** A custom theme's name and colours. Colour changes save after a pause,
 *  since a colour picker reports every step of a drag. */
function CustomEditor({ theme, onChange, onDelete }: { theme: CustomTheme; onChange: (c: Partial<CustomTheme>) => void; onDelete: () => void }) {
  const [name, setName] = useState(theme.name);
  const [colors, setColors] = useState(theme.colors);
  const first = useRef(true);
  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    const id = setTimeout(() => onChange({ colors }), 300);
    return () => clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [colors]);
  return (
    <div className="flex flex-col gap-3 rounded-card border border-line p-3">
      <div className="flex items-end gap-2">
        <Field label={t("appearance.custom.name")} className="w-64">
          <Input
            value={name}
            maxLength={100}
            onChange={(e) => setName(e.target.value)}
            onBlur={() => (name.trim() ? onChange({ name: name.trim() }) : setName(theme.name))}
          />
        </Field>
        <Button variant="danger" onClick={onDelete}>
          {t("appearance.custom.delete")}
        </Button>
      </div>
      <div className="grid grid-cols-2 sm:grid-cols-4 gap-2">
        {PALETTE_KEYS.map((k) => (
          <label key={k} className="flex items-center gap-2 text-xs">
            {/* A colour input takes #rrggbb; a selection's alpha is kept. */}
            <input
              type="color"
              value={(colors[k] ?? "#000000").slice(0, 7)}
              onChange={(e) => setColors((c) => ({ ...c, [k]: e.target.value + (c[k] ?? "").slice(7) }))}
              className="size-6 shrink-0 rounded border border-line bg-transparent"
            />
            {t(("palette." + k) as MessageKey)}
          </label>
        ))}
      </div>
    </div>
  );
}
