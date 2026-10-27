import { useEffect, useState } from "react";
import * as api from "../../api";
import type { DbNode } from "../../api";
import { t } from "../../i18n";
import { focusRing } from "../../ui/Button";
import { CollapsedIcon, ExpandedIcon, FolderIcon, TableIcon } from "../../ui/icons";
import { Spinner } from "../../ui/kit";

// The node kinds that open their rows: SQL tables and views, Redis keys,
// MongoDB collections and Elasticsearch indices.
const OPENS = ["table", "view", "key", "collection", "index"];

/** One level of the tree, loaded in pages of 200 when first shown. */
type Level = { nodes: DbNode[]; more: boolean; loading: boolean; error?: string };

/**
 * The database's objects: schemas, tables and views, and their columns,
 * each level loaded on demand from Go's catalog queries (KTD23). A table or
 * view opens its rows.
 */
export function ObjectTree({ databaseId, onOpen }: { databaseId: string; onOpen: (path: string[]) => void }) {
  const [levels, setLevels] = useState<Record<string, Level>>({});
  const [open, setOpen] = useState<Set<string>>(new Set());
  const id = (path: string[] | null) => JSON.stringify(path ?? []);

  const load = async (path: string[] | null, offset: number) => {
    const k = id(path);
    setLevels((l) => ({ ...l, [k]: { ...(l[k] ?? { nodes: [], more: false }), loading: true, error: undefined } }));
    try {
      const r = await api.databaseTree(databaseId, path, offset);
      setLevels((l) => ({ ...l, [k]: { nodes: (offset ? l[k]?.nodes ?? [] : []).concat(r.nodes), more: r.more, loading: false } }));
    } catch (e) {
      setLevels((l) => ({ ...l, [k]: { ...(l[k] ?? { nodes: [], more: false }), loading: false, error: api.errorInfo(e).message } }));
    }
  };

  useEffect(() => {
    load(null, 0);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [databaseId]);

  const toggle = (n: DbNode) => {
    const k = id(n.path);
    const next = new Set(open);
    if (next.has(k)) next.delete(k);
    else {
      next.add(k);
      if (!levels[k]) load(n.path, 0);
    }
    setOpen(next);
  };

  const level = (path: string[] | null, depth: number) => {
    const l = levels[id(path)];
    if (!l) return null;
    return (
      <ul role="group" className="flex flex-col">
        {l.nodes.map((n) => {
          const k = id(n.path);
          const expanded = open.has(k);
          const table = OPENS.includes(n.kind);
          const Icon = table ? TableIcon : n.kind === "column" ? null : FolderIcon;
          return (
            <li key={k} role="treeitem" aria-expanded={n.hasChildren ? expanded : undefined}>
              <div className="flex items-center gap-1 text-sm min-w-0" style={{ paddingLeft: depth * 12 }}>
                {n.hasChildren ? (
                  <button
                    type="button"
                    className={`shrink-0 rounded text-muted hover:text-fg ${focusRing}`}
                    aria-label={expanded ? t("database.collapse", { name: n.name }) : t("database.expand", { name: n.name })}
                    onClick={() => toggle(n)}
                  >
                    {expanded ? <ExpandedIcon className="size-3.5" aria-hidden /> : <CollapsedIcon className="size-3.5" aria-hidden />}
                  </button>
                ) : (
                  <span className="w-3.5 shrink-0" />
                )}
                {Icon && <Icon className="size-3.5 shrink-0 text-muted" aria-hidden />}
                {table ? (
                  <button
                    type="button"
                    className={`truncate text-left rounded px-0.5 hover:bg-hover ${focusRing}`}
                    title={t("database.openTable", { name: n.name })}
                    onClick={() => onOpen(n.path)}
                  >
                    {n.name}
                  </button>
                ) : (
                  <span className="truncate px-0.5" title={n.name}>
                    {n.name}
                  </span>
                )}
                {n.detail && <span className="text-xs text-muted truncate">{n.detail}</span>}
              </div>
              {expanded && level(n.path, depth + 1)}
            </li>
          );
        })}
        {l.loading && (
          <li className="py-1" style={{ paddingLeft: depth * 12 + 16 }}>
            <Spinner />
          </li>
        )}
        {l.error && (
          <li className="text-xs text-danger allow-select break-words py-1" style={{ paddingLeft: depth * 12 + 16 }}>
            {l.error}
          </li>
        )}
        {l.more && !l.loading && (
          <li style={{ paddingLeft: depth * 12 + 16 }}>
            <button type="button" className={`text-xs text-accent hover:underline rounded ${focusRing}`} onClick={() => load(path, l.nodes.length)}>
              {t("database.treeMore")}
            </button>
          </li>
        )}
        {!l.loading && !l.error && !l.nodes.length && depth === 0 && <li className="text-xs text-muted py-1">{t("database.treeEmpty")}</li>}
      </ul>
    );
  };

  return (
    <div aria-label={t("database.tree")} role="tree" className="p-2 overflow-auto h-full">
      {level(null, 0)}
    </div>
  );
}
