// Imports from other tools (R54), kept beside api.ts.
import { HostService } from "./bindings/hostbay/internal/app";
import type { Report } from "./bindings/hostbay/internal/importer/models";

export type ImportTool = "mobaxterm" | "mremoteng" | "securecrt" | "xshell" | "windterm" | "virtviewer" | "uniterm" | "kubeconfig" | "navicat" | "dbeaver";

/** What an import added, the entries it skipped and why, and notes on what
 *  imported but needs a look (a password left behind, plaintext or
 *  weakening options imported as the tool had them). */
export type ImportReport = Report;

/** Imports `tool`'s export at `path`. `password` is the tool's master or
 *  export password where it encrypts saved passwords, else empty. Hosts
 *  already saved are not added again. */
export const importFrom = (tool: ImportTool, path: string, password: string): Promise<ImportReport> =>
  HostService.ImportFrom(tool, path, password);
