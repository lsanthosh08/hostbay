import { useState } from "react";
import type { ConflictChoice } from "../api";
import { t } from "../i18n";
import { finished, useTransferActions, useTransfers, type Task } from "../state/transfers";
import { Button } from "../ui/Button";
import { IconButton } from "../ui/IconButton";
import { CloseIcon, StopIcon } from "../ui/icons";
import { humanSize } from "../tabs/files/paths";

const STATE_LABEL = {
  running: "transfer.running",
  conflict: "transfer.waiting",
  done: "transfer.done",
  failed: "transfer.failed",
  cancelled: "transfer.cancelled",
} as const;

const TONE = { running: "text-muted", conflict: "text-warning", done: "text-success", failed: "text-danger", cancelled: "text-muted" };

/** The question a task waits on: its file exists at the destination. */
function ConflictPrompt({ task }: { task: Task }) {
  const { answer } = useTransferActions();
  const [all, setAll] = useState(false);
  const c = task.conflict!;
  const pick = (choice: ConflictChoice) => answer(c.requestId, choice, all);
  return (
    <div role="group" aria-label={t("transfer.conflictTitle")} className="flex flex-col gap-2 rounded-control bg-warning-soft p-2.5">
      <p className="text-sm allow-select break-words">
        {t("transfer.conflict", {
          path: c.path,
          size: humanSize(c.size),
          date: c.mtime ? new Date(c.mtime * 1000).toLocaleString() : "?",
        })}
      </p>
      <div className="flex flex-wrap items-center gap-2">
        {/* No answer is the default: Overwrite replaces the file at the destination. */}
        <Button size="sm" onClick={() => pick("overwrite")}>
          {t("transfer.overwrite")}
        </Button>
        <Button size="sm" onClick={() => pick("skip")}>
          {t("transfer.skip")}
        </Button>
        <Button size="sm" onClick={() => pick("rename")}>
          {t("transfer.rename")}
        </Button>
        <label className="flex items-center gap-1.5 text-sm ml-1">
          <input type="checkbox" className="accent-accent size-4" checked={all} onChange={(e) => setAll(e.target.checked)} />
          {t("transfer.applyAll")}
        </label>
      </div>
    </div>
  );
}

/** A list of paths under a summary, closed until asked for. */
export function PathList({ title, items }: { title: string; items: { path: string; error?: string }[] }) {
  if (!items.length) return null;
  return (
    <details className="text-xs">
      <summary className="cursor-pointer text-muted hover:text-fg w-fit">{title}</summary>
      <ul className="mt-1 ml-4 flex flex-col gap-0.5 max-h-32 overflow-auto allow-select">
        {items.map((x) => (
          <li key={x.path} className="break-all">
            <span className="font-mono">{x.path}</span>
            {x.error && <span className="text-danger"> {x.error}</span>}
          </li>
        ))}
      </ul>
    </details>
  );
}

function TaskRow({ task }: { task: Task }) {
  const { cancel, dismiss } = useTransferActions();
  const done = finished(task);
  const pct = task.total ? Math.min(100, Math.round((task.bytes / task.total) * 100)) : done ? 100 : 0;
  const skipped = task.skipped ?? [];
  const failed = task.failed ?? [];
  return (
    <li className="flex flex-col gap-1.5 px-3 py-2.5">
      <div className="flex items-center gap-2">
        <p className="text-sm font-medium flex-1 min-w-0 truncate" title={task.label}>
          {task.label}
        </p>
        <span className={`text-xs shrink-0 ${TONE[task.state]}`}>{t(STATE_LABEL[task.state])}</span>
        {done ? (
          <IconButton size="sm" label={t("transfer.dismiss")} icon={CloseIcon} onClick={() => dismiss(task.taskId)} />
        ) : (
          <Button size="sm" variant="ghost" icon={StopIcon} onClick={() => cancel(task.taskId)}>
            {t("transfer.cancel")}
          </Button>
        )}
      </div>
      <div
        role="progressbar"
        aria-label={task.label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct}
        className="h-1.5 rounded-full bg-control-bg overflow-hidden"
      >
        <div
          className={`h-full rounded-full ${task.state === "failed" ? "bg-danger" : task.state === "cancelled" ? "bg-muted" : "bg-accent"}`}
          style={{ width: `${pct}%` }}
        />
      </div>
      <p className="text-xs text-muted tabular-nums flex gap-3 min-w-0">
        <span className="shrink-0">
          {t("transfer.files", { n: task.files, of: task.fileCount })} · {humanSize(task.bytes)} / {humanSize(task.total)}
        </span>
        {!done && task.current && <span className="truncate font-mono">{task.current}</span>}
      </p>
      {task.error && <p className="text-xs text-danger allow-select break-words">{task.error}</p>}
      {task.state === "conflict" && task.conflict && <ConflictPrompt task={task} />}
      <PathList title={t("transfer.skippedList", { n: skipped.length })} items={skipped.map((path) => ({ path }))} />
      <PathList title={t("transfer.failedList", { n: failed.length })} items={failed} />
    </li>
  );
}

/** The transfers drawer under the session area: each task's progress with
 *  Cancel, its conflict question in place, and what was skipped or failed. */
export function TransferPanel() {
  const { tasks, clear, setOpen } = useTransfers();
  return (
    <section aria-label={t("transfer.title")} className="shrink-0 max-h-[40vh] flex flex-col border-t border-line panel">
      <div className="h-9 shrink-0 px-3 flex items-center gap-2">
        <h2 className="text-sm font-semibold flex-1">{t("transfer.title")}</h2>
        <Button size="sm" variant="ghost" disabled={!tasks.some(finished)} onClick={clear}>
          {t("transfer.clear")}
        </Button>
        <IconButton size="sm" label={t("transfer.hide")} icon={CloseIcon} onClick={() => setOpen(false)} />
      </div>
      {tasks.length ? (
        <ul className="flex-1 min-h-0 overflow-auto divide-y divide-line border-t border-line">
          {[...tasks].reverse().map((task) => (
            <TaskRow key={task.taskId} task={task} />
          ))}
        </ul>
      ) : (
        <p className="px-3 pb-3 text-sm text-muted">{t("transfer.none")}</p>
      )}
    </section>
  );
}
