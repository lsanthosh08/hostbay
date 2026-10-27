import { hostKind, type Host } from "../api";
import type { Connection } from "../state/sessions";
import {
  BucketIcon,
  DatabaseFileIcon,
  DatabaseIcon,
  DockerIcon,
  DocumentStoreIcon,
  FilesIcon,
  FtpIcon,
  KeyValueIcon,
  KubernetesIcon,
  MoshIcon,
  RemoteDesktopIcon,
  SearchIndexIcon,
  SerialIcon,
  SmbIcon,
  SpiceIcon,
  SshIcon,
  TcpIcon,
  TelnetIcon,
  TerminalIcon,
  VncIcon,
  WebDavIcon,
  type Icon,
} from "../ui/icons";

const BY_KIND = {
  ssh: SshIcon,
  rdp: RemoteDesktopIcon,
  vnc: VncIcon,
  spice: SpiceIcon,
  telnet: TelnetIcon,
  mosh: MoshIcon,
  tcp: TcpIcon,
  ftp: FtpIcon,
  smb: SmbIcon,
  webdav: WebDavIcon,
  s3: BucketIcon,
  postgres: DatabaseIcon,
  sqlite: DatabaseFileIcon,
  kubernetes: KubernetesIcon,
  redis: KeyValueIcon,
  mongodb: DocumentStoreIcon,
  elasticsearch: SearchIndexIcon,
  mysql: DatabaseIcon,
  mssql: DatabaseIcon,
  oracle: DatabaseIcon,
  rqlite: DatabaseIcon,
} as const;

/** The icon for a host's connection kind. */
export const hostIcon = (h: Host): Icon => BY_KIND[hostKind(h)];

/** The icon for a session: its host's kind, or a local shell or serial port. */
export const sessionIcon = (c: Connection): Icon =>
  c.term?.kind === "local" || c.term?.kind === "container" || c.term?.kind === "pod"
    ? TerminalIcon
    : c.engine
      ? DockerIcon
    : c.term?.kind === "serial"
      ? SerialIcon
      : c.files
        ? FilesIcon
        : c.x11
          ? VncIcon
          : hostIcon(c.host);
