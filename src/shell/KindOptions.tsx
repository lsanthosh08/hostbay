import type { HostKind } from "../api";
import { t } from "../i18n";
import { KIND_GROUPS, KIND_LABEL } from "./hostForm";

/** The connection kinds as a select's grouped options, shared by the host
 *  editor's Type dropdown and the host list's filter so the two never
 *  disagree. `extra` adds attributes to one kind's option. */
export function KindOptions({ extra }: { extra?: (k: HostKind) => { disabled?: boolean; title?: string } }) {
  return KIND_GROUPS.map((g) => (
    <optgroup key={g.label} label={t(g.label)}>
      {g.kinds.map((k) => (
        <option key={k} value={k} {...extra?.(k)}>
          {t(KIND_LABEL[k])}
        </option>
      ))}
    </optgroup>
  ));
}
