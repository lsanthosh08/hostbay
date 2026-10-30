import type { MessageKey } from "../../i18n";
import type { ImportTool } from "../../importApi";

/** The apps Settings › Backup imports from (R54): what file each takes and,
 *  where the app encrypts saved passwords, what its password is called.
 *  Tool names are product names, so they are not translated. */
export const IMPORT_TOOLS: {
  id: ImportTool;
  name: string;
  extensions: string[];
  file: MessageKey;
  /** The file field's label, when the file is not an export. */
  path?: MessageKey;
  password?: MessageKey;
}[] = [
  { id: "mobaxterm", name: "MobaXterm", extensions: ["mxtsessions", "ini"], file: "import.file.mobaxterm", password: "import.password.mobaxterm" },
  { id: "mremoteng", name: "mRemoteNG", extensions: ["xml"], file: "import.file.mremoteng", password: "import.password.mremoteng" },
  { id: "securecrt", name: "SecureCRT", extensions: ["xml"], file: "import.file.securecrt", password: "import.password.securecrt" },
  { id: "xshell", name: "Xshell", extensions: ["xts", "xsh"], file: "import.file.xshell", password: "import.password.xshell" },
  { id: "windterm", name: "WindTerm", extensions: ["sessions"], file: "import.file.windterm", password: "import.password.windterm" },
  { id: "virtviewer", name: "virt-viewer", extensions: ["vv"], file: "import.file.virtviewer" },
  { id: "uniterm", name: "uniterm", extensions: ["utm"], file: "import.file.uniterm", password: "import.password.uniterm" },
  { id: "kubeconfig", name: "kubeconfig", extensions: ["yaml", "yml", "config", "kubeconfig"], file: "import.file.kubeconfig", path: "import.path.kubeconfig" },
  { id: "navicat", name: "Navicat", extensions: ["ncx"], file: "import.file.navicat" },
  { id: "dbeaver", name: "DBeaver", extensions: ["json"], file: "import.file.dbeaver", path: "import.path.dbeaver" },
  // Profiles from these open the VPN import review rather than saving at once (R42).
  { id: "vpn", name: "VPN", extensions: ["ovpn", "conf", "xml", "mobileconfig", "nmconnection", "pbk"], file: "import.file.vpn" },
  { id: "tunnelhub", name: "TunnelHub", extensions: ["json"], file: "import.file.tunnelhub" },
];
