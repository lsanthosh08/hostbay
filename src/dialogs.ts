// Native file pickers on the Wails runtime, shaped like the Tauri dialog API
// the tabs were written against. A cancelled dialog resolves to null.
import { Dialogs } from "@wailsio/runtime";

type Filter = { name: string; extensions: string[] };

// An empty extension meant "files without one" in Tauri; "*" keeps them pickable.
const toWails = (filters?: Filter[]) =>
  filters?.map((f) => ({
    DisplayName: f.name,
    Pattern: f.extensions.map((e) => (e ? `*.${e}` : "*")).join(";"),
  }));

export const open = async (opts: { filters?: Filter[] } = {}) =>
  (await Dialogs.OpenFile({ CanChooseFiles: true, Filters: toWails(opts.filters) })) || null;

export const save = async (opts: { defaultPath?: string; filters?: Filter[] } = {}) =>
  (await Dialogs.SaveFile({ Filename: opts.defaultPath, Filters: toWails(opts.filters) })) || null;

// A folder, slash-separated as the transfer service takes local paths.
export const openFolder = async () => {
  const dir = await Dialogs.OpenFile({ CanChooseDirectories: true, CanChooseFiles: false });
  return typeof dir === "string" && dir ? dir.replace(/\\/g, "/") : null;
};
