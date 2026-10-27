import { useEffect, useState, type TextareaHTMLAttributes } from "react";
import * as api from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { controlCls, Field, useFieldControl } from "../../ui/Field";
import { Spinner } from "../../ui/kit";
import { indentJSON } from "./rows";

// A multi-line field, named by its Field label like Input.
function TextArea(props: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...useFieldControl(props)} {...props} className={`w-full px-3 py-2 font-mono text-xs ${controlCls}`} />;
}

/** Why text is not JSON, or undefined when it is; parsed only to check. */
function jsonError(text: string): string | undefined {
  try {
    JSON.parse(text);
    return undefined;
  } catch (e) {
    return (e as Error).message;
  }
}

/**
 * Edits one document whole, as JSON (KTD23): Go sends it in full, since a
 * grid cell may be cut, and replaces it by its _id, which the page never
 * changes. MongoDB documents arrive as canonical Extended JSON, so every
 * value keeps its type when saved.
 */
export function DocumentView({
  databaseId,
  path,
  id,
  onClose,
  onSaved,
}: {
  databaseId: string;
  path: string[];
  id: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [loaded, setLoaded] = useState<string | null>(null);
  const [text, setText] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  // Loaded once per document, so a re-render never replaces an edit.
  const where = JSON.stringify(path);
  useEffect(() => {
    api.databaseDocument(databaseId, path, id).then(
      (d) => {
        const doc = indentJSON(d);
        setLoaded(doc);
        setText(doc);
      },
      (e) => setError(api.errorInfo(e).message)
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [databaseId, where, id]);

  const dirty = loaded !== null && text !== loaded;
  const invalid = loaded !== null ? jsonError(text) : undefined;
  const save = async () => {
    setBusy(true);
    setError(undefined);
    try {
      await api.databaseSaveDocument(databaseId, path, id, text);
      onSaved();
    } catch (e) {
      setError(api.errorInfo(e).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={t("database.editDocument")}
      size="xl"
      dismissible={!dirty}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant="primary" disabled={!dirty || !!invalid || busy} onClick={save}>
            {t("database.documentSave")}
          </Button>
        </>
      }
    >
      {loaded === null && !error ? (
        <Spinner />
      ) : (
        <Field
          label={`_id ${id}`}
          hint={t("database.documentHint")}
          error={invalid ? t("database.documentInvalid", { error: invalid }) : error}
        >
          {/* The value stays text: nothing here is rendered as HTML. */}
          <TextArea rows={18} spellCheck={false} value={text} aria-invalid={!!(invalid || error)} onChange={(e) => setText(e.target.value)} />
        </Field>
      )}
    </Dialog>
  );
}
