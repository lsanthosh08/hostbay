import type { DbCell, DbColumn } from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { isJSON } from "./grid";
import { cellText, indentJSON } from "./rows";

/**
 * One cell's value in full as the page holds it: JSON laid out, any other
 * text as it is. A cut or binary cell says so and offers its full value,
 * which Go writes to a file, since the page holds only what it shows.
 */
export function ValueView({ column, cell, onSave, onClose }: { column: DbColumn; cell: DbCell; onSave: () => void; onClose: () => void }) {
  const partial = cell.c || cell.b;
  const text = cell.n ? "NULL" : partial ? cellText(cell) : isJSON(cell.t) ? indentJSON(cell.t) : cell.t;
  return (
    <Dialog
      title={`${column.name} (${column.type})`}
      size="xl"
      onClose={onClose}
      footer={
        <>
          {partial && <Button onClick={onSave}>{t("database.saveValue")}</Button>}
          <Button onClick={() => navigator.clipboard.writeText(cell.n ? "" : cell.t).catch(() => {})}>{t("database.copyValue")}</Button>
          <Button variant="primary" onClick={onClose}>
            {t("common.close")}
          </Button>
        </>
      }
    >
      {partial && <p className="text-sm text-warning mb-2">{t("database.valuePartial")}</p>}
      {/* The value stays text: nothing here is rendered as HTML. */}
      <pre className={`text-xs font-mono whitespace-pre-wrap break-words allow-select ${cell.n ? "text-muted italic" : ""}`}>{text}</pre>
    </Dialog>
  );
}
