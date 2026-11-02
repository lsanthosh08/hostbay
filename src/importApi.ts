// Imports from other tools (R54), kept beside api.ts.
import { Events } from "@wailsio/runtime";
import type { Vpn } from "./api";
import { HostService } from "./bindings/hostbay/internal/app";
import type { BrowserMapping, Report, Skipped } from "./bindings/hostbay/internal/importer/models";
import type { Found } from "./bindings/hostbay/internal/importer/vpnconf/models";

/** The host importers, and "vpn" and "tunnelhub", which go through the VPN
 *  review flow instead of importFrom. */
export type ImportTool = "mobaxterm" | "mremoteng" | "securecrt" | "xshell" | "windterm" | "virtviewer" | "uniterm" | "kubeconfig" | "navicat" | "dbeaver" | "vpn" | "tunnelhub";
export const isVpnTool = (tool: ImportTool): tool is "vpn" | "tunnelhub" => tool === "vpn" || tool === "tunnelhub";

/** What an import added, the entries it skipped and why, and notes on what
 *  imported but needs a look (a password left behind, plaintext or
 *  weakening options imported as the tool had them). VPN imports add the
 *  profiles saved, each with the directives dropped (R43), and the browser
 *  mappings saved with them. */
export type ImportReport = Report;

/** Imports `tool`'s export at `path`. `password` is the tool's master or
 *  export password where it encrypts saved passwords, else empty. Hosts
 *  already saved are not added again. */
export const importFrom = (tool: Exclude<ImportTool, "vpn" | "tunnelhub">, path: string, password: string): Promise<ImportReport> =>
  HostService.ImportFrom(tool, path, password);

/** One profile a VPN import read, with no secret; hasSecret says the file held some. */
export type VpnDraft = { profile: Vpn; ignored: string[] | null; askPassword: boolean };
export type VpnImportPreview = { token: string; drafts: VpnDraft[]; skipped: Skipped[] | null; notes: string[] | null; browsers: BrowserMapping[] | null };
/** A configuration an installed VPN client keeps (R39); an unreadable one is listed by name only. */
export type FoundVpnConfig = Found;

/** Reads a VPN configuration file or bundle, or a TunnelHub vpn-config.json,
 *  without saving anything (R38, R41). */
export const previewVpnImport = (path: string) =>
  Promise.resolve(HostService.PreviewVPNImport(path)).then((p) => ({ ...p, drafts: p.drafts ?? [] }) as VpnImportPreview);
/** Saves the kept drafts with their review edits and closes the import;
 *  an empty list saves nothing. */
export const commitVpnImport = (token: string, keep: Vpn[]): Promise<ImportReport> => HostService.CommitVPNImport(token, keep);
/** Forgets a previewed file, and the secrets read from it, when its review is closed unsaved. */
export const cancelVpnImport = (token: string) => {
  try {
    void Promise.resolve(HostService.CancelVPNImport(token)).catch(() => {});
  } catch {
    // the preview expires by itself
  }
};
export const scanVpnConfigs = () => Promise.resolve(HostService.ScanVPNConfigs()).then((r) => r ?? []);

/** Files dropped on an element marked data-file-drop-target, by path. */
export const onFilesDropped = (cb: (paths: string[]) => void): (() => void) => Events.On("files-dropped", (e) => cb((e.data as string[] | null) ?? []));
