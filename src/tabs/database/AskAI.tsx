import { useState } from "react";
import * as api from "../../api";
import { t } from "../../i18n";
import { Button } from "../../ui/Button";
import { Input } from "../../ui/Field";
import { AIIcon } from "../../ui/icons";

/** A question to SQL for a database tab (R64). Go sends the question with
 *  the open database's table and column names only, and the SQL that
 *  comes back goes into the editor. Nothing runs until the user presses
 *  Run; an error leaves the editor as it was. */
export function AskAI({ databaseId, onSQL }: { databaseId: string; onSQL: (sql: string) => void }) {
  const [question, setQuestion] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const ask = async () => {
    const q = question.trim();
    if (!q || busy) return;
    setBusy(true);
    setError("");
    try {
      onSQL(await api.aiAskSql(databaseId, q));
    } catch (e) {
      setError(api.errorInfo(e).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <form
      className="flex flex-wrap items-center gap-2 px-2 py-1.5 border-b border-line"
      onSubmit={(e) => {
        e.preventDefault();
        ask();
      }}
    >
      <Input
        className="flex-1 min-w-48 h-8"
        aria-label={t("database.ask.label")}
        aria-describedby="db-ask-note"
        placeholder={t("database.ask.placeholder")}
        value={question}
        onChange={(e) => setQuestion(e.target.value)}
      />
      <Button size="sm" type="submit" icon={AIIcon} disabled={busy || !question.trim()}>
        {busy ? t("database.ask.busy") : t("database.ask.button")}
      </Button>
      <p id="db-ask-note" role={error ? "alert" : undefined} className={`basis-full text-xs ${error ? "text-danger allow-select break-words" : "text-muted"}`}>
        {error || t("database.ask.hint")}
      </p>
    </form>
  );
}
