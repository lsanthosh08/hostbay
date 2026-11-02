// Native file pickers on the Wails runtime, shaped like the Tauri dialog API
// the tabs were written against. A cancelled dialog resolves to null.
import { Dialogs, System } from "@wailsio/runtime";

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

// A program to run. On a Mac an app is a folder, and an "app" file filter
// greys every app out in the panel, so it picks folders and the caller checks
// the name; on Windows it is an .exe.
export const openApp = async () => {
  const mac = System.IsMac();
  const filters = System.IsWindows() ? [{ name: "Programs", extensions: ["exe"] }] : undefined;
  const path = (await Dialogs.OpenFile({ CanChooseFiles: true, CanChooseDirectories: mac, Directory: mac ? "/Applications" : undefined, Filters: toWails(filters) })) || null;
  if (typeof path === "string" && mac && !/\.app\/?$/i.test(path)) throw new Error("Choose an app, such as Check Point SASE.app");
  return typeof path === "string" ? path.replace(/\/$/, "") : null;
};
