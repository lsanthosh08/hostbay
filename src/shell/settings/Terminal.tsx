import { useEffect, useState } from "react";
import { t } from "../../i18n";
import { loadSettings, setAppearance, settings } from "../../state/settings";
import { Select } from "../../ui/Select";
import { Group } from "./Group";

const SIZES = [11, 12, 13, 14, 15, 16, 18, 20];

/** The terminal font size, a synced setting; new terminals use it
 *  (src/tabs/TerminalTab.tsx). */
export const termFontSize = () => settings().fontSize || Number(localStorage.getItem("termFontSize")) || 13;

/** The terminal font size. Its colours are under Appearance. */
export function Terminal() {
  const [size, setSize] = useState(termFontSize);
  useEffect(() => {
    loadSettings().then(() => setSize(termFontSize()), () => {});
  }, []);
  return (
    <Group title={t("settings.termFont")} description={t("settings.termFontHint")}>
      <Select
        className="w-32"
        aria-label={t("settings.termFont")}
        value={size}
        onChange={(e) => {
          const n = Number(e.target.value);
          setSize(n);
          setAppearance({ fontSize: n }).catch(() => {});
        }}
      >
        {SIZES.map((n) => (
          <option key={n} value={n}>
            {n} px
          </option>
        ))}
      </Select>
    </Group>
  );
}
