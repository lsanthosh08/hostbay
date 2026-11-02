// Typed seam between React and the Go backend (Wails services in internal/app).
// Each wrapper keeps the old Tauri command's name and payload shape.
import { Events, Stream } from "@wailsio/runtime";
import type { KeyOverrides } from "./actions/shortcuts";
import {
  AdminService,
  AIService,
  AppearanceService,
  ConnService,
  DatabaseService,
  FilesService,
  HostService,
  KubernetesService,
  MasterPasswordService,
  MonitorService,
  RdpService,
  SettingsService,
  SpiceService,
  SyncService,
  TerminalToolsService,
  TransferService,
  TunnelService,
  UpdateService,
  VncService,
  WorkspaceService,
  X11Service,
  ZmodemService,
  MCPService,
  VPNService,
  NetdService,
  RoutingService,
  TunnelHubService,
} from "./bindings/hostbay/internal/app";

export type UnlistenFn = () => void;

// Go sends a nil slice as null; the tabs expect an array.
const list = <T>(p: PromiseLike<T[] | null>): Promise<T[]> => Promise.resolve(p).then((r) => r ?? []);

// Subscribe to a backend event; resolves to its unsubscribe function.
const on = <T>(name: string, cb: (data: T) => void): Promise<UnlistenFn> =>
  Promise.resolve(Events.On(name, (e) => cb(e.data as T)));

/** What a failed backend call carries (see internal/app/errors.go). */
export type AppError = { code?: string; message: string; details?: Record<string, string> };

/** Reads the structured error from a rejected backend call. */
export function errorInfo(e: unknown): AppError {
  // Wails rejects with a RuntimeError whose `cause` is MarshalError's output.
  const cause = (e as { cause?: AppError } | undefined)?.cause;
  if (cause && typeof cause.message === "string") return cause;
  return { message: e instanceof Error ? e.message : String(e) };
}

// In master-password mode (Linux without a Secret Service) the vault is
// locked after a restart, and opening or saving a secret cannot ask for the
// password itself. A call that fails as locked unlocks through the backend's
// master password prompt and runs once more; a cancelled prompt rejects.
async function unlocking<T>(call: () => Promise<T>): Promise<T> {
  try {
    return await call();
  } catch (e) {
    const info = errorInfo(e);
    if (info.code !== "SecretNeeded" || info.details?.reason !== "locked") throw e;
    await MasterPasswordService.UnlockVault();
    return call();
  }
}

// ---- Types (mirror the Go structs in internal/app/types.go) ----

export type Host = {
  id: string;
  alias: string;
  hostname: string;
  port: number;
  user: string;
  identityFile: string; // path to private key
  tags: string[];
  lastConnected?: string;
  folderId?: string | null;
  order?: number;
  // agent: the local SSH agent's keys; kerberos: a ticket from kinit.
  authType?: "key" | "password" | "agent" | "kerberos" | null;
  password?: string | null;
  hasPassword?: boolean;
  // telnet and mosh hosts open a terminal only; telnet is unencrypted.
  // ftp, smb, webdav and s3 are file hosts: they open the Files view only.
  kind?: HostKind | null;
  rdpPort?: number | null;
  // RDP security: "auto" (TLS+CredSSP), "tls" (no NLA — for xrdp/Linux), "nla".
  rdpSecurity?: "auto" | "tls" | "nla" | null;
  // MAC address for Wake-on-LAN (auto-detected on connect).
  mac?: string | null;
  // Host this one is reached through (ProxyJump), by host id.
  jumpHostId?: string | null;
  // File hosts. user and password are the sign-in (S3: access key ID and
  // secret key); an empty user is anonymous FTP, a guest share or a public
  // WebDAV folder. A port of 0 is the protocol's default.
  // ftp: null is explicit. vnc: the security mode, null is "vencrypt";
  // "none" is plain VNC, unencrypted unless a jump host carries it. spice:
  // null is "tls" (the port is a SPICE tls-port); "none" is a plain port,
  // likewise unencrypted unless a jump host carries it. postgres, redis, mongodb, mysql, mssql
  // and oracle: null is "verify" (TLS, the certificate checked or pinned); "none" is
  // unencrypted. elasticsearch and rqlite have none: their url says.
  tls?: "explicit" | "implicit" | "vencrypt" | "tls" | "verify" | "none" | null;
  url?: string | null; // webdav: the folder; s3: the endpoint; elasticsearch and rqlite: the node
  domain?: string | null; // smb
  bucket?: string | null; // s3
  region?: string | null; // s3; null is us-east-1
  pathStyle?: boolean | null; // s3: on for MinIO and other self-hosted servers
  // Connection extras (R48–R52). A host with a proxy dials only through it;
  // a missing or failed proxy is an error, never a direct connection.
  proxyId?: string | null;
  // The VPN profile every dial goes through, never with a proxy (R15, KTD5).
  vpnId?: string | null;
  forwardAgent?: boolean | null; // ssh; off unless set
  algorithms?: SshAlgorithms | null; // ssh; legacy algorithms on top of the defaults
  keepalive?: number | null; // ssh; seconds, 0 or unset is 30
  localEcho?: boolean | null; // tcp
  // ssh: X11 forwarding, off unless set, and the X11 desktop's command;
  // null runs the first of startxfce4, mate-session or gnome-session (R47).
  x11?: boolean | null;
  x11Desktop?: string | null;
  // Database hosts (R56). database: the database name. file: a SQLite host's
  // path, shown only; saving sets it from filePick, the token databasePickFile
  // returned, and never from a path the page sends.
  database?: string | null;
  file?: string | null;
  filePick?: string | null;
  // Kubernetes hosts (R57): url is the API server, namespace the one a tab
  // opens on. The CA and TLS server name come from a kubeconfig import and
  // stay in Go; password is the bearer token.
  namespace?: string | null;
};

export type SshAlgorithms = { kex?: string[] | null; ciphers?: string[] | null; macs?: string[] | null; hostKeys?: string[] | null };

/** A SOCKS5 or HTTP CONNECT proxy (Settings > Proxies). Its password is never
 *  sent back: hasPassword says one is saved, and an empty one keeps it. */
export type Proxy = {
  id: string;
  name: string;
  type: "socks5" | "http";
  host: string;
  port: number;
  user?: string;
  password?: string | null;
  hasPassword?: boolean;
};

// tcp: a raw TCP terminal, unencrypted, like Telnet without the protocol.
// vnc: a remote screen; Go signs in with the password (KTD19), port 0 is 5900.
// spice: a KVM or QEMU machine's screen, signed in the same way (R46); it has
// no user, and port 0 is 5900.
export type HostKind = "ssh" | "rdp" | "telnet" | "mosh" | "tcp" | "vnc" | "spice" | FileKind | DatabaseKind | "kubernetes";
export type FileKind = "ftp" | "smb" | "webdav" | "s3";
export const FILE_KINDS: readonly FileKind[] = ["ftp", "smb", "webdav", "s3"];
/** A file host opens the Files view only, over the transfer service. */
export const isFileHost = (h: Pick<Host, "kind">) => FILE_KINDS.includes(h.kind as FileKind);
// postgres: port 0 is 5432. sqlite: a database file on this computer.
// redis: port 0 is 6379, database its number; a password needs no user.
// mongodb: port 0 is 27017, database the auth database; no user signs in to
// nothing. elasticsearch: a node by url, with no tls mode (http:// is
// unencrypted); no user signs in to nothing. mysql, mssql and oracle: port 0
// is 3306, 1433 or 1521; oracle's database is its service name. rqlite: a
// node by url, like elasticsearch; no user signs in to nothing.
export type DatabaseKind = "postgres" | "sqlite" | "redis" | "mongodb" | "elasticsearch" | "mysql" | "mssql" | "oracle" | "rqlite";
export const DATABASE_KINDS: readonly DatabaseKind[] = ["postgres", "sqlite", "redis", "mongodb", "elasticsearch", "mysql", "mssql", "oracle", "rqlite"];
/** A database host opens the database view only, over DatabaseService. */
export const isDatabaseHost = (h: Pick<Host, "kind">) => DATABASE_KINDS.includes(h.kind as DatabaseKind);
export const hostKind = (h: Pick<Host, "kind">): HostKind => h.kind ?? "ssh";

export type Folder = {
  id: string;
  name: string;
  parentId?: string | null;
  order?: number;
};

export type ExecResult = { stdout: string; stderr: string; code: number | null };

export type DirEntry = {
  name: string;
  path: string;
  isDir: boolean;
  size: number;
  mode: number; // unix perms
  mtime: number; // epoch secs
  owner: number;
  group: number;
};

// Details of a HostKeyUnknown error. The UI answers yes/no to requestId only.
// hop > 0 is a jump host; kind "rdpCert" is an RDP server certificate.
export type HostKeyPrompt = {
  host: string;
  port: string;
  keyType: string;
  fingerprint: string;
  requestId: string;
  hop?: string;
  kind?: string;
};

// Details of a HostKeyChanged refusal from connect. removeId is what
// removeHostKey takes; certificate refusals have none.
export type HostKeyChange = {
  host: string;
  port: string;
  hop: string;
  keyType: string;
  fingerprint: string;
  knownFingerprints: string;
  removeId: string;
  // "rdpCert", "vncCert", "ftpCert", "webdavCert" or "s3Cert" for a certificate.
  kind?: string;
};

export type DockerContainer = {
  id: string;
  name: string;
  image: string;
  status: string;
  state: string;
  ports: string;
};

export type DockerImage = {
  id: string;
  repository: string;
  tag: string;
  size: string;
  created: string;
};

export type DockerStat = {
  id: string;
  name: string;
  cpu: string;
  mem: string;
  memPerc: string;
};

export type ComposeProject = { name: string; path: string };

export type SystemdUnit = { unit: string; load: string; active: string; sub: string; description: string };

export type DashboardInfo = {
  os: string;
  uptime: string;
  load: string;
  disk: string;
  nginxInstalled: boolean;
  dockerInstalled: boolean;
  dockerPermission: boolean;
  podmanInstalled: boolean;
  nerdctlInstalled: boolean;
  azureAgentInstalled: boolean;
};

// ---- Host store ----
export const listHosts = () => list(HostService.ListHosts()) as Promise<Host[]>;
export const saveHost = (host: Host) => unlocking(() => HostService.SaveHost(host) as Promise<Host>);
export const deleteHost = (id: string) => HostService.DeleteHost(id);
export const listProxies = () => list(HostService.ListProxies()) as Promise<Proxy[]>;
export const saveProxy = (proxy: Proxy) => unlocking(() => HostService.SaveProxy(proxy) as Promise<Proxy>);
export const deleteProxy = (id: string) => HostService.DeleteProxy(id);
export const listTrash = () => list(HostService.ListTrash()) as Promise<Host[]>;
export const cloneHost = (id: string) => unlocking(() => HostService.CloneHost(id) as Promise<Host>);
export const restoreHost = (id: string) => HostService.RestoreHost(id);
export const purgeHost = (id: string) => HostService.PurgeHost(id);
// clearRecent empties the Recent list; connecting stamps it server-side.
export const clearRecent = () => HostService.ClearRecent();
// Resolves to { imported, notes }: notes name ProxyJump aliases that matched no host.
export const importSshConfig = (path?: string) => HostService.ImportSshConfig(path ?? null);
// Backup: with a passphrase, encrypted and including passwords + keys; with an
// empty one, plain JSON with every secret removed. Both need OS verification.
export const exportStore = (path: string, passphrase: string) =>
  HostService.ExportStore(path, passphrase);
export const exportHost = (id: string, path: string, passphrase: string) =>
  HostService.ExportHost(id, path, passphrase);
export const writeTextFile = (path: string, content: string) =>
  HostService.WriteTextFile(path, content);
export const readTextFile = (path: string) => HostService.ReadTextFile(path);
// Restore auto-detects a Hostbay backup or plain JSON.
// Resolves to { imported, notes }: notes name hosts that need a jump host or
// proxy chosen.
export const importStore = (path: string, passphrase?: string) =>
  HostService.ImportStore(path, passphrase ?? null);
export const revealPassword = (id: string) => HostService.RevealPassword(id);

// ---- Master password (Linux user verification, KTD7) ----
// Only Linux verifies with a Hostbay master password; elsewhere
// needsMasterPassword is always false. Setting one is its own action and
// refuses to replace an existing password.
export const needsMasterPassword = () => MasterPasswordService.NeedsMasterPassword();
export const setMasterPassword = (password: string) => MasterPasswordService.SetMasterPassword(password);
/** A backend call is waiting for the master password; answer by requestId. */
export type MasterPasswordRequest = { requestId: string; reason: string };
export const onMasterPasswordPrompt = (cb: (r: MasterPasswordRequest) => void) =>
  on<MasterPasswordRequest>("master-password-prompt", cb);
// An empty password cancels; the waiting call then fails verification.
export const answerMasterPassword = (requestId: string, password: string) =>
  MasterPasswordService.AnswerMasterPassword(requestId, password);
// Wake-on-LAN: broadcast a magic packet from this Mac, or relay via an SSH host.
export const wolWake = (mac: string) => AdminService.WolWake(mac);
export const wolWakeVia = (relaySessionId: string, mac: string) =>
  AdminService.WolWakeVia(relaySessionId, mac);
export const listSshKeys = () => list(HostService.ListSshKeys());
// Copied identity keys (dropdown in the host form).
export const saveIdentityKey = (sourcePath: string) => unlocking(() => HostService.SaveIdentityKey(sourcePath));
export const listIdentityKeys = () => list(HostService.ListIdentityKeys());
export const deleteIdentityKey = (name: string) => HostService.DeleteIdentityKey(name);
export const listFolders = () => list(HostService.ListFolders()) as Promise<Folder[]>;
export const saveFolder = (folder: Folder) => HostService.SaveFolder(folder) as Promise<Folder>;
export const deleteFolder = (id: string, mode: "reparent" | "purge" = "reparent") =>
  HostService.DeleteFolder(id, mode);

// ---- Connection ----
export const connect = (hostId: string) => unlocking(() => ConnService.Connect(hostId));
export const connectAdhoc = (
  hostname: string,
  port: number,
  user: string,
  identityFile: string
) => ConnService.ConnectAdhoc(hostname, port, user, identityFile);
// Answer a HostKeyUnknown prompt; the backend keeps the key it captured.
export const trustHostKey = (requestId: string, accept: boolean) =>
  ConnService.TrustHostKey(requestId, accept);
// Remove the old key behind a HostKeyChanged refusal. The backend asks for OS
// verification and refuses unless typedHost is the refused host's name.
export const removeHostKey = (removeId: string, typedHost: string) =>
  ConnService.RemoveHostKey(removeId, typedHost);
export const disconnect = (sessionId: string) => ConnService.Disconnect(sessionId);
// Rebuilds a dropped session's shared connection; its views then reopen.
export const reconnect = (sessionId: string) => ConnService.Reconnect(sessionId);
// known_hosts for Settings. Hashed entries hide their host and port, so only
// named ones can be removed; removal asks for OS verification and refuses
// unless typedHost is the host name.
export type KnownHost = { host: string; port: number; hashed: boolean; keyType: string; fingerprint: string };
export const listKnownHosts = () => list(ConnService.ListKnownHosts()) as Promise<KnownHost[]>;
export const removeKnownHost = (host: string, port: number, typedHost: string) =>
  ConnService.RemoveKnownHost(host, port, typedHost);

// Tests the editor's unsaved form (R53) and saves nothing. step is "ok" or
// the step that failed: proxy, tcp, ssh (negotiation), hostKey or auth.
// database: signed in, but the database itself refused (databaseTest).
export type ConnTest = { step: "ok" | "proxy" | "vpn" | "tcp" | "ssh" | "hostKey" | "auth" | "database"; message?: string; error?: AppError | null };
export const testConnection = (host: Host) => unlocking(() => ConnService.TestConnection(host) as Promise<ConnTest>);

// ---- Exec ----
export const exec = (sessionId: string, command: string) =>
  ConnService.ExecCommand(sessionId, command) as Promise<ExecResult>;

// ---- PTY / terminal ----
export const ptyOpen = (sessionId: string, cols: number, rows: number) =>
  ConnService.PtyOpen(sessionId, cols, rows);
// Telnet needs no SSH session: ownerId is the tab's own ID, which disconnect
// takes like a session ID.
export const ptyOpenTelnet = (ownerId: string, hostId: string, cols: number, rows: number) =>
  ConnService.PtyOpenTelnet(ownerId, hostId, cols, rows);
// A raw TCP host's terminal; ownerId as for Telnet. It has no window size.
export const ptyOpenTCP = (ownerId: string, hostId: string) => ConnService.PtyOpenTCP(ownerId, hostId);
// Starts mosh-server over the session's SSH connection, then switches to UDP.
export const ptyOpenMosh = (sessionId: string, hostId: string, cols: number, rows: number) =>
  ConnService.PtyOpenMosh(sessionId, hostId, cols, rows);
// Local shells and serial ports need no session either: ownerId is the tab's
// own ID, as for Telnet. Only listed shell IDs and port names open.
export type LocalShell = { id: string; name: string };
export const listLocalShells = () => list<LocalShell>(ConnService.ListLocalShells());
export const ptyOpenLocal = (ownerId: string, shellId: string, cols: number, rows: number) =>
  ConnService.PtyOpenLocal(ownerId, shellId, cols, rows);
export type SerialSettings = {
  port: string;
  baud: number;
  dataBits: number;
  parity: "none" | "odd" | "even" | "mark" | "space";
  stopBits: "1" | "1.5" | "2";
  localEcho: boolean;
};
export const listSerialPorts = () => list<string>(ConnService.ListSerialPorts());
export const ptyOpenSerial = (ownerId: string, s: SerialSettings) =>
  ConnService.PtyOpenSerial(ownerId, s.port, s.baud, s.dataBits, s.parity, s.stopBits, s.localEcho);
export const ptyWrite = (ptyId: string, data: string) => ConnService.PtyWrite(ptyId, data);
export const ptyResize = (ptyId: string, cols: number, rows: number) =>
  ConnService.PtyResize(ptyId, cols, rows);
export const ptyClose = (ptyId: string) => ConnService.PtyClose(ptyId);
// Output is held until ptyAttach, so subscribe first, then attach.
export const ptyAttach = (ptyId: string) => ConnService.PtyAttach(ptyId);
export const onPtyData = (ptyId: string, cb: (data: string) => void) =>
  on<string>(`pty://${ptyId}`, cb);
/** Fires once, after the last output, with why the terminal ended. */
export const onPtyClosed = (ptyId: string, cb: (reason: string) => void) =>
  on<string>(`pty-closed://${ptyId}`, cb);
/** "dropped" when the session's connection ends, "live" once it is rebuilt. */
export const onConnState = (sessionId: string, cb: (state: "live" | "dropped") => void) =>
  on<"live" | "dropped">(`conn-state://${sessionId}`, cb);

// ---- Zmodem (R43): src/tabs/zmodem.ts runs it; ZmodemStreamName in internal/app/zmodem.go ----
/** Fires when `sz` ("download") or `rz` ("upload") starts in an SSH terminal. */
export const onZmodem = (ptyId: string, cb: (direction: "download" | "upload") => void) =>
  on<"download" | "upload">(`zmodem://${ptyId}`, cb);
/** The folder a download saves into, or "" when the user cancelled. */
export const zmodemChooseFolder = (ptyId: string) => ZmodemService.ZmodemChooseFolder(ptyId);
export type ZmodemFile = { name: string; size: number };
/** The files an upload sends, or none when the user cancelled. */
export const zmodemChooseFiles = (ptyId: string) => list(ZmodemService.ZmodemChooseFiles(ptyId));
// Go's []byte arrives as base64; empty at the end of the file.
export const zmodemReadFile = (ptyId: string, index: number, offset: number, length: number) =>
  ZmodemService.ZmodemReadFile(ptyId, index, offset, length).then((b64) =>
    Uint8Array.from(atob(b64 ?? ""), (c) => c.charCodeAt(0))
  );
export const zmodemCancel = (ptyId: string) => ZmodemService.ZmodemCancel(ptyId);
export const zmodemStream = () => Stream("zmodem");

// ---- SFTP / files ----
export const sftpList = (sessionId: string, path: string) =>
  list(FilesService.SftpList(sessionId, path)) as Promise<DirEntry[]>;
export const sftpRead = (sessionId: string, path: string) => FilesService.SftpRead(sessionId, path);
export const sftpWrite = (
  sessionId: string,
  path: string,
  content: string,
  sudo: boolean,
  sudoPassword?: string
) => FilesService.SftpWrite(sessionId, path, content, sudo, sudoPassword ?? null);
export const sftpMkdir = (sessionId: string, path: string) =>
  FilesService.SftpMkdir(sessionId, path);
export const sftpRename = (sessionId: string, from: string, to: string) =>
  FilesService.SftpRename(sessionId, from, to);
export const sftpRemove = (sessionId: string, path: string, isDir: boolean, sudo: boolean) =>
  FilesService.SftpRemove(sessionId, path, isDir, sudo);
export const sftpCopy = (sessionId: string, from: string, to: string, sudo: boolean) =>
  FilesService.SftpCopy(sessionId, from, to, sudo);
export const sftpChmod = (sessionId: string, path: string, mode: number, sudo: boolean) =>
  FilesService.SftpChmod(sessionId, path, mode, sudo);
export const sftpDownload = (sessionId: string, remote: string, local: string) =>
  FilesService.SftpDownload(sessionId, remote, local);
export const sftpUpload = (sessionId: string, local: string, remote: string) =>
  FilesService.SftpUpload(sessionId, local, remote);

// ---- Transfers (U17, U30) ----
// A source is "local" (this computer), a session ID from connect (its SFTP,
// or SCP when the server refuses SFTP), or a file host's ID from
// transferConnect. Paths are slash-separated on every source.
export type TransferSource = { id: string; kind: string; home?: string };
export type TransferConflict = { requestId: string; path: string; size: number; mtime: number };
export type TransferState = "running" | "conflict" | "done" | "failed" | "cancelled";
/** One task's state, at most every 100 ms while it runs; the last event
 *  carries the totals. */
export type TransferEvent = {
  taskId: string;
  state: TransferState;
  bytes: number;
  total: number;
  files: number;
  fileCount: number;
  current?: string;
  skipped?: string[];
  failed?: { path: string; error: string }[];
  conflict?: TransferConflict;
  error?: string;
};
export type ConflictChoice = "overwrite" | "skip" | "rename";
export const transferLocal = "local";
// A file host's certificate prompt rejects with HostKeyUnknown or
// HostKeyChanged (details.kind ftpCert, webdavCert or s3Cert), as connect does.
export const transferConnect = (hostId: string) =>
  unlocking(() => TransferService.TransferConnect(hostId) as Promise<TransferSource>);
export const transferDisconnect = (sourceId: string) => TransferService.TransferDisconnect(sourceId);
export const transferTrustCert = (requestId: string, accept: boolean) =>
  TransferService.TransferTrustCert(requestId, accept);
export const transferOpen = (sourceId: string) => TransferService.TransferOpen(sourceId) as Promise<TransferSource>;
export const transferList = (sourceId: string, dir: string) =>
  list(TransferService.TransferList(sourceId, dir)) as Promise<DirEntry[]>;
export const transferStart = (fromId: string, paths: string[], toId: string, toDir: string) =>
  TransferService.TransferStart(fromId, paths, toId, toDir);
export const transferCancel = (taskId: string) => TransferService.TransferCancel(taskId);
export const transferAnswer = (requestId: string, choice: ConflictChoice, all: boolean) =>
  TransferService.TransferAnswer(requestId, choice, all);
export const transferMkdir = (sourceId: string, dir: string, name: string) =>
  TransferService.TransferMkdir(sourceId, dir, name);
export const transferRename = (sourceId: string, from: string, to: string) =>
  TransferService.TransferRename(sourceId, from, to);
export const transferRemove = (sourceId: string, path: string, isDir: boolean) =>
  TransferService.TransferRemove(sourceId, path, isDir);
export const onTransfer = (cb: (e: TransferEvent) => void) => on<TransferEvent>("transfer", cb);

// ---- Docker ----
export const dockerPs = (sessionId: string) =>
  list(AdminService.DockerPs(sessionId)) as Promise<DockerContainer[]>;
export const dockerAction = (sessionId: string, id: string, action: string) =>
  AdminService.DockerAction(sessionId, id, action);
export const dockerImages = (sessionId: string) =>
  list(AdminService.DockerImages(sessionId)) as Promise<DockerImage[]>;
export const dockerPull = (sessionId: string, image: string) =>
  AdminService.DockerPull(sessionId, image) as Promise<ExecResult>;
export const dockerRmi = (sessionId: string, id: string) => AdminService.DockerRmi(sessionId, id);
export const dockerPrune = (sessionId: string, what: string) =>
  AdminService.DockerPrune(sessionId, what) as Promise<ExecResult>;
export const dockerStats = (sessionId: string) =>
  list(AdminService.DockerStats(sessionId)) as Promise<DockerStat[]>;
export const dockerSystemDf = (sessionId: string) => AdminService.DockerSystemDf(sessionId);
export const composeProjects = (sessionId: string, extraPaths: string[]) =>
  list(AdminService.ComposeProjects(sessionId, extraPaths)) as Promise<ComposeProject[]>;
export const composeAction = (sessionId: string, path: string, action: string) =>
  AdminService.ComposeAction(sessionId, path, action) as Promise<ExecResult>;
export const dockerFixPermission = (sessionId: string) =>
  AdminService.DockerFixPermission(sessionId) as Promise<ExecResult>;
export const dockerLogs = (sessionId: string, id: string, tail: string) => AdminService.DockerLogs(sessionId, id, tail);
export const dockerCreate = (sessionId: string, o: CreateOptions) => AdminService.DockerCreate(sessionId, o);
// The password goes to the engine once, on its stdin; Hostbay keeps nothing.
export const dockerLogin = (sessionId: string, registry: string, user: string, password: string) =>
  AdminService.DockerLogin(sessionId, registry, user, password);
export const containerListDir = (sessionId: string, id: string, path: string) =>
  AdminService.ContainerListDir(sessionId, id, path);
export const containerReadFile = (sessionId: string, id: string, path: string) =>
  AdminService.ContainerReadFile(sessionId, id, path);
// A terminal on a container: a shell (exec -it), or with follow its logs.
// ownerId is the terminal tab's own ID, as for a local shell.
export const ptyOpenContainer = (
  ownerId: string,
  sessionId: string,
  id: string,
  shell: string,
  follow: boolean,
  cols: number,
  rows: number
) => AdminService.ContainerTerminal(ownerId, sessionId, id, shell, follow, cols, rows);

// ---- Container engines (Docker on this computer, Podman, nerdctl, WSLC) ----
export type ContainerEngine = { id: string; label: string; path?: string };
export type ContainerCaps = {
  restart: boolean;
  restartPolicy: boolean;
  compose: boolean;
  prune: boolean;
  diskUsage: boolean;
  namespaces: boolean;
};
export type ContainerInfo = {
  engine: string;
  kind: string;
  namespace: string;
  local: boolean;
  path: string;
  context: string;
  endpoint: string;
  caps: ContainerCaps;
  engines: ContainerEngine[];
};
export type PortOption = { hostAddress: string; hostPort: number; containerPort: number; protocol: string };
export type MountOption = { source: string; target: string; readOnly: boolean };
export type CreateOptions = {
  image: string;
  name: string;
  ports: PortOption[];
  env: string[];
  volumes: MountOption[];
  binds: MountOption[];
  restart: string;
};
export const containerLocalEngines = () => list(AdminService.ContainerLocalEngines()) as Promise<ContainerEngine[]>;
// Opens the stand-in session for an engine on this computer; containerClose ends it.
export const containerOpenLocal = (engineId: string) => AdminService.ContainerOpenLocal(engineId);
export const containerClose = (sessionId: string) => AdminService.ContainerClose(sessionId);
export const containerInfo = (sessionId: string) =>
  AdminService.ContainerInfo(sessionId).then((i) => ({ ...i, engines: i?.engines ?? [] }) as ContainerInfo);
export const containerUse = (sessionId: string, engineId: string, namespace: string) =>
  AdminService.ContainerUse(sessionId, engineId, namespace);
export const containerNamespaces = (sessionId: string) => list(AdminService.ContainerNamespaces(sessionId));

// ---- Services ----
export const nginxTest = (sessionId: string) =>
  AdminService.NginxTest(sessionId) as Promise<ExecResult>;
export const nginxReload = (sessionId: string) =>
  AdminService.NginxReload(sessionId) as Promise<ExecResult>;
export const systemdList = (sessionId: string) =>
  list(AdminService.SystemdList(sessionId)) as Promise<SystemdUnit[]>;
export const systemdAction = (sessionId: string, unit: string, action: string) =>
  AdminService.SystemdAction(sessionId, unit, action) as Promise<ExecResult>;

// ---- Dashboard ----
export const dashboard = (sessionId: string) =>
  AdminService.DashboardInfo(sessionId) as Promise<DashboardInfo>;

// ---- Scripts (persisted command buttons) ----
export type Script = { id: string; label: string; script: string };
export const listScripts = () => list(HostService.ListScripts()) as Promise<Script[]>;
export const saveScripts = (scripts: Script[]) => HostService.SaveScripts(scripts);
export const listBookmarks = () => list(HostService.ListBookmarks());
export const saveBookmarks = (bookmarks: string[]) => HostService.SaveBookmarks(bookmarks);

// ---- Keys ----
export const ppkToPem = (ppkPath: string, outPath: string) =>
  ConnService.PpkToPem(ppkPath, outPath);

// ---- IIS / Windows ----
export type IisSite = {
  name: string;
  id: number;
  state: string;
  physicalPath: string;
  bindings: string;
  appPool: string;
};
export type IisPool = { name: string; state: string; runtime: string };
export type WinDashboard = {
  os: string;
  uptime: string;
  cpu: string;
  memory: string;
  disk: string;
  iisInstalled: boolean;
};
export const iisSites = (sessionId: string) =>
  list(AdminService.IisSites(sessionId)) as Promise<IisSite[]>;
export const iisPools = (sessionId: string) =>
  list(AdminService.IisPools(sessionId)) as Promise<IisPool[]>;
export const iisSiteAction = (sessionId: string, name: string, action: string) =>
  AdminService.IisSiteAction(sessionId, name, action) as Promise<ExecResult>;
export const iisPoolAction = (sessionId: string, name: string, action: string) =>
  AdminService.IisPoolAction(sessionId, name, action) as Promise<ExecResult>;
export const windowsDashboard = (sessionId: string) =>
  AdminService.WindowsDashboard(sessionId) as Promise<WinDashboard>;

// ---- RDP (Windows remote desktop) ----
// Every RDP call and event is keyed by the rdpId that rdpConnect returns.
// rdpConnect rejects with HostKeyUnknown / HostKeyChanged carrying
// details.kind "rdpCert" for the server certificate (answer with
// rdpTrustCert, then connect again), or without it for a jump host's SSH key
// (answer with trustHostKey).
export type RdpFrame = { x: number; y: number; width: number; height: number; rgba: Uint8ClampedArray<ArrayBuffer> };
export const rdpConnect = (hostId: string, width: number, height: number) =>
  unlocking(() => RdpService.RdpConnect(hostId, width, height));
export const rdpTrustCert = (requestId: string, accept: boolean) =>
  RdpService.RdpTrustCert(requestId, accept);
export const rdpClose = (rdpId: string) => RdpService.RdpClose(rdpId);
export const rdpInput = (
  rdpId: string,
  kind: "move" | "button" | "wheel" | "key",
  opts: { x?: number; y?: number; button?: number; down?: boolean; scancode?: number; delta?: number }
) =>
  RdpService.RdpInput(
    rdpId,
    kind,
    opts.x ?? 0,
    opts.y ?? 0,
    opts.button ?? 0,
    opts.down ?? false,
    opts.scancode ?? 0,
    opts.delta ?? 0
  );
// Frames, desktop size and the session's end arrive over the "rdp" GoStream
// (internal/rdp: MsgFrame=1, MsgSize=2, MsgClosed=3; uint16 little-endian).
// The backend holds them from connect until this subscribes. onClosed gets
// the reason, empty when the user closed the session.
export function rdpView(
  rdpId: string,
  h: {
    onFrame: (f: RdpFrame) => void;
    onSize: (s: { width: number; height: number }) => void;
    onClosed: (reason: string) => void;
  }
): UnlistenFn {
  const ws = Stream("rdp");
  let ended = false;
  const end = (reason: string) => {
    if (!ended) {
      ended = true;
      h.onClosed(reason);
    }
  };
  ws.binaryType = "arraybuffer";
  ws.onopen = () => ws.send(rdpId);
  ws.onmessage = (ev: MessageEvent) => {
    const buf = ev.data as ArrayBuffer;
    const b = new Uint8Array(buf);
    const v = new DataView(buf);
    const u16 = (i: number) => v.getUint16(i, true);
    if (b[0] === 1 && b.length >= 9) {
      const width = u16(5), height = u16(7);
      const rgba = new Uint8ClampedArray(buf, 9);
      if (rgba.length === width * height * 4) h.onFrame({ x: u16(1), y: u16(3), width, height, rgba });
    } else if (b[0] === 2 && b.length >= 5) {
      h.onSize({ width: u16(1), height: u16(3) });
    } else if (b[0] === 3) {
      end(new TextDecoder().decode(b.subarray(1)));
    }
  };
  ws.onclose = () => end("The RDP connection to Hostbay closed.");
  return () => {
    ended = true;
    ws.close();
  };
}
export const rdpSetClipboard = (rdpId: string, text: string) =>
  RdpService.RdpSetClipboard(rdpId, text);
export const onRdpClipboard = (rdpId: string, cb: (text: string) => void) =>
  on<string>(`rdp-clipboard://${rdpId}`, cb);
export const onRdpClipFiles = (rdpId: string, cb: (paths: string[]) => void) =>
  on<string[]>(`rdp-clip-files://${rdpId}`, cb);

// ---- VNC (R45, KTD19) ----
// vncConnect signs in in Go and returns a vncId that works once, for 30 s.
// It rejects with HostKeyUnknown / HostKeyChanged (details.kind "vncCert")
// for a VeNCrypt certificate (answer with vncTrustCert, then connect
// again), or SecretNeeded when the server refused the password.
export const vncConnect = (hostId: string) => unlocking(() => VncService.VncConnect(hostId));
export const vncTrustCert = (requestId: string, accept: boolean) => VncService.VncTrustCert(requestId, accept);
export const vncClose = (vncId: string) => VncService.VncClose(vncId);
/** The "vnc" GoStream (app.VncStreamName). Send the vncId first; after it the
 *  stream is plain RFB from a server that asks for no password. Closing it
 *  hangs up on the server. */
export const vncStream = () => Stream("vnc");

// ---- SPICE (R46, KTD19) ----
// spiceConnect signs in to the main channel in Go and returns a spiceId; the
// main channel's stream must open within 30 s. It rejects with
// HostKeyUnknown / HostKeyChanged (details.kind "spiceCert") for a TLS
// port's certificate (answer with spiceTrustCert, then connect again), or
// SecretNeeded when the server refused the password (none saved sends an
// empty one). spiceClose ends the session and every channel.
export const spiceConnect = (hostId: string) => unlocking(() => SpiceService.SpiceConnect(hostId));
export const spiceTrustCert = (requestId: string, accept: boolean) => SpiceService.SpiceTrustCert(requestId, accept);
export const spiceClose = (spiceId: string) => SpiceService.SpiceClose(spiceId);
/** One "spice" GoStream (app.SpiceStreamName) per SPICE channel, main first.
 *  Send the spiceId first; after it the stream is that channel, linked by
 *  spice-html5 with an empty password (Go signs in). Closing the main
 *  channel's stream ends the session. */
export const spiceStream = () => Stream("spice");

// ---- Databases (R56, KTD23) ----
// databaseConnect opens a database host in Go and returns its databaseId and
// capabilities (the page shows controls by capability, never by engine). It
// rejects with HostKeyUnknown (details.kind "<kind>Cert", such as
// "postgresCert" or "redisCert"; answer with
// databaseTrustCert, then connect again), HostKeyChanged with a removeId for
// databaseRemovePin, or SecretNeeded when no password is saved or the
// database refused it. Errors are DatabaseError with details.category.
export type DatabaseInfo = { databaseId: string; capabilities: string[] };
export type DbNode = { kind: string; name: string; path: string[]; hasChildren: boolean; detail?: string };
export type DbColumn = { name: string; type: string; binary?: boolean };
/** A cell: t its text (a binary value's hex prefix), n NULL, c cut at 64 KiB,
 *  b binary, s its full size in bytes when cut or binary. */
export type DbCell = { t: string; n?: boolean; c?: boolean; b?: boolean; s?: number };
/** Up to 500 rows of result set `result`, from row `row`; columns arrive
 *  with a result set's first page. done: the last page of the last set. */
export type DbPage = { result: number; columns?: DbColumn[] | null; row: number; rows: DbCell[][]; done: boolean };
/** edit: the only columns the engine changes (none named: every one);
 *  noNull: the engine refuses NULL. */
export type DbTable = { path: string[]; key: string[]; edit?: string[]; noNull?: boolean };
export type QueryState = { queryId: string; state: "running" | "done" | "error" | "cancelled" | "expired"; rows: number; message?: string; rerun?: boolean };
export const databaseConnect = (hostId: string) =>
  unlocking(() => DatabaseService.DatabaseConnect(hostId) as Promise<DatabaseInfo>);
// Checks a PostgreSQL host from the editor form, as testConnection does.
export const databaseTest = (host: Host) => unlocking(() => DatabaseService.DatabaseTest(host) as Promise<ConnTest>);
export const databaseTrustCert = (requestId: string, accept: boolean) => DatabaseService.DatabaseTrustCert(requestId, accept);
// Needs the refused host name typed and OS verification (VerifyFailed when cancelled).
export const databaseRemovePin = (removeId: string, typedHost: string) => DatabaseService.DatabaseRemovePin(removeId, typedHost);
export const databaseClose = (databaseId: string) => DatabaseService.DatabaseClose(databaseId);
export const databaseTree = (databaseId: string, path: string[] | null, offset: number) =>
  DatabaseService.DatabaseTree(databaseId, path, offset).then((r) => ({ nodes: (r?.nodes ?? []) as DbNode[], more: !!r?.more }));
// Runs text as one request and returns its queryId at once; state arrives on
// onQueryState and rows from databaseNextPage.
export const databaseQuery = (databaseId: string, text: string) => DatabaseService.DatabaseQuery(databaseId, text);
// Opens a table from the tree; table is null when it has no key and opens read-only.
export const databaseOpenTable = (databaseId: string, path: string[]) =>
  DatabaseService.DatabaseOpenTable(databaseId, path) as Promise<{ queryId: string; table: DbTable | null }>;
export const databaseNextPage = (queryId: string) => DatabaseService.DatabaseNextPage(queryId) as Promise<DbPage>;
export const databaseCancel = (queryId: string) => DatabaseService.DatabaseCancel(queryId);
export const databaseCloseQuery = (queryId: string) => DatabaseService.DatabaseCloseQuery(queryId);
// Changes one row by the table's own key; null values are NULL.
export const databaseEditRow = (databaseId: string, path: string[], key: Record<string, string | null>, set: Record<string, string | null>) =>
  DatabaseService.DatabaseEditRow(databaseId, { path, key, set });
// With the documentEdit capability, a collection or index opens like a table
// keyed by "_id": databaseDocument returns a document in full as JSON (a
// grid cell may be cut), and databaseSaveDocument replaces it by its _id.
export const databaseDocument = (databaseId: string, path: string[], id: string) =>
  DatabaseService.DatabaseDocument(databaseId, path, id) as Promise<string>;
export const databaseSaveDocument = (databaseId: string, path: string[], id: string, doc: string) =>
  DatabaseService.DatabaseSaveDocument(databaseId, path, id, doc);
// Writes a cut or binary cell in full to a file chosen in Go's dialog;
// false when the user cancelled.
export const databaseSaveCell = (queryId: string, result: number, row: number, col: number) =>
  DatabaseService.DatabaseSaveCell(queryId, result, row, col);
// Asks for a SQLite file in Go's dialog; token is what a saved host's
// filePick takes. A cancelled dialog returns an empty token.
export const databasePickFile = () => DatabaseService.DatabasePickFile() as Promise<{ token: string; path: string }>;
export const onQueryState = (queryId: string, cb: (s: QueryState) => void) => on<QueryState>(`query-state://${queryId}`, cb);

// ---- Kubernetes (R57, KTD28) ----
// kubernetesConnect opens a cluster in Go and returns its clusterId. An
// unknown self-signed certificate fails HostKeyUnknown (details.kind
// "kubernetesCert": kubernetesTrustCert, then connect again), a changed one
// HostKeyChanged with a removeId for kubernetesRemovePin, and a refused
// token SecretNeeded. The token never reaches the page.
export type ClusterInfo = { clusterId: string; context: string; namespace: string; server: string; version: string };
// One object as the list shows it. Secrets carry key names only, never values.
export type K8sItem = {
  name: string;
  namespace?: string;
  created?: string;
  labels?: Record<string, string>;
  status?: string;
  ready?: string;
  restarts?: number;
  containers?: string[];
  keys?: string[];
  type?: string;
  detail?: string;
};
export type K8sPage = { items: K8sItem[]; continue: string };
export type K8sApplied = { kind: string; name: string; namespace?: string; outcome: string };
export type K8sUsage = { name: string; namespace?: string; cpu: number; memory: number };
// installed false: the cluster has no metrics.k8s.io ("not installed").
export type K8sMetrics = { installed: boolean; pods: K8sUsage[]; nodes: K8sUsage[] };
export const kubernetesConnect = (hostId: string) => unlocking(() => KubernetesService.KubernetesConnect(hostId) as Promise<ClusterInfo>);
export const kubernetesClose = (clusterId: string) => KubernetesService.KubernetesClose(clusterId);
export const kubernetesTrustCert = (requestId: string, accept: boolean) => KubernetesService.KubernetesTrustCert(requestId, accept);
export const kubernetesRemovePin = (removeId: string, typedHost: string) => KubernetesService.KubernetesRemovePin(removeId, typedHost);
// kind: namespaces, nodes, pods, services, configmaps, secrets, events,
// deployments, statefulsets, daemonsets, jobs or ingresses. namespace ""
// lists every namespace; cont is the previous page's continue token.
export const kubernetesList = (clusterId: string, kind: string, namespace: string, cont: string): Promise<K8sPage> =>
  KubernetesService.KubernetesList(clusterId, kind, namespace, cont).then((p) => ({ items: (p?.items ?? []) as K8sItem[], continue: p?.continue ?? "" }));
export const kubernetesGet = (clusterId: string, kind: string, namespace: string, name: string) =>
  KubernetesService.KubernetesGet(clusterId, kind, namespace, name);
export const kubernetesRevealSecret = (clusterId: string, namespace: string, name: string, key: string) =>
  KubernetesService.KubernetesRevealSecret(clusterId, namespace, name, key);
export const kubernetesDelete = (clusterId: string, kind: string, namespace: string, name: string) =>
  KubernetesService.KubernetesDelete(clusterId, kind, namespace, name);
export const kubernetesScale = (clusterId: string, kind: string, namespace: string, name: string, replicas: number) =>
  KubernetesService.KubernetesScale(clusterId, kind, namespace, name, replicas);
export const kubernetesRestart = (clusterId: string, kind: string, namespace: string, name: string) =>
  KubernetesService.KubernetesRestart(clusterId, kind, namespace, name);
export const kubernetesApply = (clusterId: string, namespace: string, yaml: string, dryRun: boolean) =>
  list(KubernetesService.KubernetesApply(clusterId, namespace, yaml, dryRun) as Promise<K8sApplied[] | null>);
export const kubernetesMetrics = (clusterId: string, namespace: string) =>
  KubernetesService.KubernetesMetrics(clusterId, namespace) as Promise<K8sMetrics>;
// Logs and exec return a ptyId: subscribe with onPtyData, then ptyAttach.
export const kubernetesLogs = (clusterId: string, namespace: string, pod: string, container: string) =>
  KubernetesService.KubernetesLogs(clusterId, namespace, pod, container);
// command is an argument list (the shell picked), never run through a shell.
export const kubernetesExec = (clusterId: string, namespace: string, pod: string, container: string, command: string[], cols: number, rows: number) =>
  KubernetesService.KubernetesExec(clusterId, namespace, pod, container, command, cols, rows);

// ---- X11 (R47, KTD20) ----
// localXServer says where this computer's X server answers for forwarded
// X11 programs, or the problem, naming the X server to install for this OS.
export type X11Status = { display?: string; problem?: string };
export const localXServer = (): Promise<X11Status> => X11Service.LocalXServer();
// x11DesktopOpen starts Xvnc and the host's X11 desktop over its SSH
// connection and returns a vncId, streamed over vncStream and ended with
// vncClose like vncConnect's (claim it within 30 s). Host-key and secret
// prompts reject as from connect; a host that cannot run the desktop (no
// Xvnc, an Xvnc without -rfbunixpath, no desktop) rejects with why.
export const x11DesktopOpen = (hostId: string) => unlocking(() => X11Service.X11DesktopOpen(hostId));

// ---- Tunnels (saved port forwards on a host) ----
// local (-L) and dynamic SOCKS (-D) listen here; remote (-R) listens on the
// host. bindHost "" means 127.0.0.1; anything beyond loopback needs allowExternal.
export type TunnelKind = "local" | "remote" | "dynamic";
export type Tunnel = {
  id: string;
  hostId: string;
  name: string;
  kind: TunnelKind;
  bindHost: string;
  bindPort: number;
  targetHost: string;
  targetPort: number;
  allowExternal: boolean;
};
// starting and listening hold a connection; failed and dropped carry error.
export type TunnelStatus = "starting" | "listening" | "failed" | "dropped" | "stopped";
export type TunnelState = { id: string; status: TunnelStatus; addr?: string; error?: string };
export const listTunnels = () => list(TunnelService.ListTunnels()) as Promise<Tunnel[]>;
export const saveTunnel = (t: Tunnel) => TunnelService.SaveTunnel(t) as Promise<Tunnel>;
export const deleteTunnel = (id: string) => TunnelService.DeleteTunnel(id);
// Rejects with HostKeyUnknown (answer with trustHostKey, then start again) or
// the reason it failed; the failed state also arrives via onTunnelState.
export const startTunnel = (id: string) =>
  TunnelService.StartTunnel(id) as Promise<TunnelState>;
export const stopTunnel = (id: string) => TunnelService.StopTunnel(id);
export const tunnelStates = () => list(TunnelService.TunnelStates()) as Promise<TunnelState[]>;
export const onTunnelState = (cb: (s: TunnelState) => void) => on<TunnelState>("tunnel-state", cb);

// ---- VPN profiles (R1, R8, R9) ----
export type VpnTier = "app" | "system" | "external";
export type VpnAuth = "password" | "certificate" | "otp" | "browser";
// type is a row of the supported-types table (src/shell/vpnForm.ts) and tier
// follows from it. Secrets are never sent back: hasSecret says some are saved,
// and an empty one keeps the saved one.
export type Vpn = {
  id: string;
  name: string;
  type: string;
  tier: string;
  gateway?: string;
  port?: number;
  user?: string;
  auth?: VpnAuth | ""; // "" is none
  authGroup?: string; // AnyConnect: the gateway group to sign in to, when it offers several
  address?: string[] | null; // WireGuard: the tunnel's own addresses
  peerKey?: string; // WireGuard: the server's public key
  routes?: string[] | null; // networks sent through the VPN; 0.0.0.0/0 and ::/0 are a full tunnel
  dnsServers?: string[] | null;
  dnsDomains?: string[] | null;
  mtu?: number; // 0 is 1280
  tags?: string[] | null;
  autoReconnect?: boolean;
  autoApply?: boolean;
  routingMode?: string;
  client?: string; // external: the installed client's name, as the OS shows it
  clientPath?: string; // external, this computer only: the client program Open client runs
  iface?: string; // external, this computer only: the tunnel interface it is pinned to
  password?: string | null;
  privateKey?: string | null;
  presharedKey?: string | null;
  authKey?: string | null;
  hasSecret?: boolean;
};
// signing_in waits for need ("otp" or "browser"); failed and dropped carry reason.
export type VpnStatus = "disconnected" | "connecting" | "signing_in" | "connected" | "failed" | "dropped";
export type VpnState = {
  id: string;
  status: VpnStatus;
  reason?: string;
  need?: string;
  address?: string[] | null;
  routes?: string[] | null;
  since?: number;
  iface?: string; // a connected external profile's tunnel interface
  url?: string; // signing_in at a browser: the sign-in page, always http or https
  code?: string; // failed: "needs-system-wide", "cert-unknown", "cert-changed" or "openconnect-version"
  serverCert?: string; // cert-unknown, cert-changed: the pin the gateway presented
};
export const listVpns = () => list(VPNService.ListVPNs()) as Promise<Vpn[]>;
export const saveVpn = (v: Vpn) => unlocking(() => VPNService.SaveVPN(v) as Promise<Vpn>);
export const deleteVpn = (id: string) => VPNService.DeleteVPN(id);
// Resolves once up; rejects with the coded reason (a type Hostbay cannot run
// yet rejects VPNFailed, reason "unsupported"), which also arrives as a state.
export const connectVpn = (id: string) => unlocking(() => VPNService.ConnectVPN(id) as Promise<VpnState>);
export const disconnectVpn = (id: string) => VPNService.DisconnectVPN(id);
export const answerVpn = (id: string, code: string) => VPNService.AnswerVPN(id, code);
// Pins the certificate an untrusted gateway presented (its last connect's
// serverCert, cert-unknown only), keeping the saved password (R21).
export const trustVpnCert = (id: string, pin: string) => unlocking(() => VPNService.TrustVPNCert(id, pin));
// Removes a profile's pinned gateway certificate after OS verification,
// keeping the saved password; the next connect asks to trust the gateway's.
// A changed certificate is never trusted in place (R21, AE8).
export const forgetVpnCert = (id: string) => unlocking(() => VPNService.ForgetVPNCert(id));
// Disconnects a Tailscale profile and forgets this computer's node, so the
// next connect signs in afresh.
export const signInAgainVpn = (id: string) => VPNService.SignInAgainVPN(id);
// The sign-in groups an AnyConnect gateway offers, for the profile form's
// list; empty when it offers none to choose from. Signs in to nothing.
export const vpnGroups = (gateway: string, port: number) => list(VPNService.VPNGroups(gateway, port)) as Promise<string[]>;
export const vpnStates = () => list(VPNService.VPNStates()) as Promise<VpnState[]>;
export const onVpnState = (cb: (s: VpnState) => void) => on<VpnState>("vpn-state", cb);
// A VPN found on this computer that no profile names yet (R22).
export type DetectedVpn = { name: string; type: string; shape?: string; connected: boolean; iface?: string; client?: string };
export type VpnTunnel = { iface: string; local: string; peer?: string; shape?: string; vpn?: string };
export const vpnTunnels = () => list(VPNService.VPNTunnels()) as Promise<VpnTunnel[]>;
export const detectVpns = () => list(VPNService.DetectVPNs()) as Promise<DetectedVpn[]>;
// Opens an external profile's client program saved on this computer (R3).
export const launchVpnClient = (id: string) => VPNService.LaunchVPNClient(id);
export const connectVpnClient = (id: string) => VPNService.ConnectVPNClient(id);
export const disconnectVpnClient = (id: string) => VPNService.DisconnectVPNClient(id);

// ---- Browser profiles through a VPN (R33) ----
export type BrowserProfile = { dir: string; name: string };
/** A Chromium-family browser on this computer and its profiles. */
export type InstalledBrowser = { id: string; name: string; profiles: BrowserProfile[] | null };
/** Launches `profile` (its folder) of `browser` through VPN profile `vpn`; kept on this computer. */
export type BrowserMapping = { browser: string; profile: string; vpn: string };
export const installedBrowsers = () => list(VPNService.InstalledBrowsers()) as Promise<InstalledBrowser[]>;
export const browserMappings = () => list(VPNService.BrowserMappings()) as Promise<BrowserMapping[]>;
export const saveBrowserMapping = (m: BrowserMapping) => VPNService.SaveBrowserMapping(m);
export const deleteBrowserMapping = (browser: string, profile: string) => VPNService.DeleteBrowserMapping(browser, profile);
/** Connects the mapped VPN if needed, which may need the vault, then starts the profile through it. */
export const launchBrowser = (browser: string, profile: string) => unlocking(() => VPNService.LaunchBrowser(browser, profile));
// Go asks for the VPN panel when a profile waits on a one-time code or a
// browser sign-in, or a connect from the tray failed.
export const onShowVpns = (cb: () => void) => on<null>("show-vpns", cb);

// ---- System routes for system-wide and external VPNs (R25 to R30) ----
// The privileged helper: every state but ready says why routes cannot apply;
// missing, repair and reinstall are fixed by installHelper (one admin prompt).
export type HelperState = "unavailable" | "missing" | "repair" | "stopped" | "reinstall" | "ready";
export type HelperStatus = { state: HelperState; reason?: string };
export const helperStatus = () => NetdService.Status() as Promise<HelperStatus>;
export const installHelper = () => NetdService.Install() as Promise<HelperStatus>;
// A planned change; gateway routes through the local network. profile is the
// VPN profile it is for, if any.
export type RouteKind = "add" | "delete" | "narrow" | "pin" | "mtu";
export type RouteChange = { kind: RouteKind; op?: string; dst?: string; iface?: string; gateway?: boolean; mtu?: number; profile?: string; reason: string };
export type RouteSkip = { dst: string; profile?: string; reason: string };
// A broad vendor route offered for narrowing, keep being the suggested subnets.
export type GreedyRoute = { dst: string; iface: string; profile: string; keep: string[] };
export type RoutePlan = { changes: RouteChange[]; skipped: RouteSkip[]; greedy: GreedyRoute[]; unlabeled: string[] };
export type RouteResult = RouteChange & { error?: string };
export type RoutingReport = { results: RouteResult[]; skipped: RouteSkip[]; partly: string[] };
// profiles: profile ID to "applied", "partly applied" or "fighting".
export type RoutingState = { profiles: Record<string, string>; reset: boolean; notice?: string };
// With id, previews or applies (and saves) that profile in mode: "" per destination, "full" or "split".
export const previewRoutes = (id = "", mode = "") => RoutingService.Preview(id, mode) as Promise<RoutePlan>;
export const applyRoutes = (id = "", mode = "") => RoutingService.Apply(id, mode) as Promise<RoutingReport>;
export const resetRoutes = () => RoutingService.Reset() as Promise<RoutingReport>;
export const narrowRoute = (id: string, greedy: string, keep: string[]) => RoutingService.Narrow(id, greedy, keep);
export const localRoutes = () => list(RoutingService.LocalRoutes()) as Promise<string[]>;
export const setLocalRoutes = (dsts: string[]) => RoutingService.SetLocalRoutes(dsts);
export const pinInterface = (id: string, iface: string) => RoutingService.PinInterface(id, iface);
export const routingState = () => RoutingService.State() as Promise<RoutingState>;
export const onRoutingState = (cb: (s: RoutingState) => void) => on<RoutingState>("routing-state", cb);

// ---- TunnelHub retirement (R45): its helper and agent, the config to import ----
export type TunnelHubItem = { kind: "daemon" | "helper" | "socket" | "agent"; path: string; admin: boolean };
export type TunnelHubFound = { items: TunnelHubItem[]; configPath?: string; appPath?: string };
// status: "removed" (Removed and Failed say per item), "declined" in Hostbay's dialog, "cancelled" at the admin prompt.
export type TunnelHubRemoval = { status: "removed" | "declined" | "cancelled"; removed: string[]; failed: string[]; error?: string };
export const detectTunnelHub = () => Promise.resolve(TunnelHubService.Detect()).then((f) => ({ ...f, items: f?.items ?? [] }) as TunnelHubFound);
export const removeTunnelHub = () => TunnelHubService.Remove() as Promise<TunnelHubRemoval>;

// ---- Server monitor (R59): one sampler per Linux host, shared by its Monitor tabs ----
// Sizes are bytes, rates bytes per second. Every string came from the host:
// render it as text. A process's cpu is -1 where ps cannot report it.
export type MonitorProc = { pid: number; user: string; cpu: number; rss: number; command: string };
export type MonitorPort = { proto: string; addr: string; port: number; process?: string };
export type MonitorIface = {
  name: string;
  state: string;
  mac: string;
  mtu: number;
  addrs: string[];
  rxRate: number;
  txRate: number;
};
export type MonitorDisk = { fs: string; mount: string; size: number; used: number };
// procs and ports are null on history samples except the newest.
export type MonitorSample = {
  time: number;
  partial?: boolean;
  cpu: number;
  cores: number;
  load: number[]; // 1, 5 and 15 minutes
  memTotal: number;
  memUsed: number;
  swapTotal: number;
  swapUsed: number;
  rxRate: number;
  txRate: number;
  disks: MonitorDisk[];
  ifaces: MonitorIface[];
  procCount: number;
  procs: MonitorProc[] | null;
  ports: MonitorPort[] | null;
};
// disconnected waits for the session's reconnect; unsupported (not Linux,
// os says what) has stopped for good.
export type MonitorState = "starting" | "live" | "error" | "disconnected" | "unsupported";
export type MonitorEvent = { state: MonitorState; error?: string; os?: string; sample?: MonitorSample };
export type MonitorStart = { id: string; state: MonitorEvent; history: MonitorSample[]; interval: number };
export const monitorStart = (sessionId: string, hostId: string) =>
  MonitorService.Start(sessionId, hostId) as Promise<MonitorStart>;
export const monitorStop = (id: string) => MonitorService.Stop(id);
/** A sampler pauses while none of its tabs is visible. */
export const monitorVisible = (id: string, visible: boolean) => MonitorService.SetVisible(id, visible);
/** Sets the host's interval for all its tabs; resolves to the clamped seconds. */
export const monitorInterval = (id: string, seconds: number) => MonitorService.SetInterval(id, seconds);
export const onMonitorSample = (id: string, cb: (e: MonitorEvent) => void) =>
  on<MonitorEvent>(`monitor-sample://${id}`, cb);

// ---- Sync (R35–R37) ----
// Everything leaves this computer encrypted with the sync passphrase.
// syncConfigure, syncChangePassphrase need OS verification and reject with
// VerifyFailed when it is cancelled; an SFTP target with an unknown host key
// rejects with HostKeyUnknown (answer with trustHostKey, then configure again).
export type SyncKind = "git" | "webdav" | "folder" | "s3" | "sftp" | "smb";
export type SyncTarget = {
  kind: SyncKind;
  url?: string;
  host?: string;
  port?: number;
  user?: string;
  path?: string;
  branch?: string;
  region?: string;
  bucket?: string;
  share?: string;
  domain?: string;
};
// kind "" holds a tunnel set to listen beyond loopback; "host" a host whose
// synced change weakens it (the fields say which); "proxy" a proxy pointed
// at a new address (endpoint); "aimode" an AI execution mode weaker than
// this computer's (mode); "postlogin" a post-login rule another computer made
// or changed, which does not run here until allowed. Any other kind is from
// a newer Hostbay: it stays
// held and this build cannot resolve it. proxyId "" removes the host's proxy.
export type SyncHold = {
  id: string;
  name: string;
  kind?: string;
  plaintext?: boolean;
  forwardAgent?: boolean;
  x11?: boolean;
  algorithms?: SshAlgorithms | null;
  proxyId?: string | null;
  endpoint?: { type: string; host: string; port: number } | null;
  // A Kubernetes host's synced server and CA (KTD28).
  cluster?: { url: string; hostname: string; port: number; caData?: string; tlsServerName?: string } | null;
  mode?: string;
};
export type SyncStatus = {
  enabled: boolean;
  target: SyncTarget;
  lastSync: string;
  lastError: string;
  // decrypt | updateNeeded | rollback | passphraseChanged | hostKeyChanged | auth, or "".
  paused: string;
  needsChoice: boolean;
  conflicts: number;
  holds: SyncHold[];
};
export type SyncReport = { uploaded: boolean; received: number; conflicts: number; retries: number; notes: string[] };
export type SyncConflict = {
  id: string;
  collection: string;
  itemId: string;
  label: string;
  kind: "edit" | "purge";
  deviceId: string;
  modifiedAt: string;
};
// Go never sends holds or notes as null.
export const syncStatus = () => SyncService.Status() as Promise<SyncStatus>;
// secret: token, password or S3 secret key; "" keeps the saved one for the same target.
export const syncConfigure = (target: SyncTarget, secret: string, passphrase: string) =>
  SyncService.Configure({ ...target, secret, passphrase });
// choice answers the first sync on a computer that already has data.
export const syncNow = (choice: "" | "remote" | "merge" = "") =>
  unlocking(() => SyncService.SyncNow(choice) as Promise<SyncReport>);
export const syncChangePassphrase = (passphrase: string) =>
  SyncService.ChangePassphrase(passphrase) as Promise<SyncReport>;
export const syncDisable = () => SyncService.Disable();
export const syncConflicts = () => list(SyncService.Conflicts()) as Promise<SyncConflict[]>;
export const syncRestoreConflict = (id: string) => SyncService.RestoreConflict(id);
export const syncDismissConflict = (id: string) => SyncService.DismissConflict(id);
// Allowing a weaker AI mode asks in Go's dialog, then OS verification; a
// post-login rule that sends a saved secret asks for OS verification. Either
// rejects with VerifyFailed when cancelled, and the hold stays.
export const syncConfirmHold = (id: string, accept: boolean) => SyncService.ConfirmHold(id, accept);

// ---- Settings (KTD37): src/state/settings.ts loads them once at start ----
// One synced group; each group uses only its own fields. Go refuses "ai"
// from the page: the AI execution mode only shows here.
export type SettingsGroup = {
  id: string;
  theme?: string;
  fontSize?: number;
  terminalTheme?: string;
  customThemes?: { id: string; name: string; type: string; colors: Record<string, string> }[];
  backgroundOpacity?: number;
  language?: string;
  executionMode?: string;
  keys?: KeyOverrides | null;
};
// This computer's own settings. The page may change only these three.
export type LocalSettings = { aiSuggestions?: boolean; hostLogging?: Record<string, boolean>; keepRunning?: boolean };
export const settingsGet = (group: string) => SettingsService.Get(group) as Promise<SettingsGroup>;
export const settingsSet = (g: SettingsGroup) => SettingsService.Set(g as Parameters<typeof SettingsService.Set>[0]);
export const settingsGetLocal = () => SettingsService.GetLocal() as Promise<LocalSettings>;
export const settingsSetLocal = (patch: LocalSettings) => SettingsService.SetLocal(patch);
// The theme and font size kept in localStorage before settings moved to Go;
// Go ignores them once any setting exists.
export const settingsImportLegacy = (theme: string, fontSize: number) => SettingsService.ImportLegacy(theme, fontSize);
// The data folder (R72). The page cannot name a folder: Go asks in its own
// dialog, then restarts Hostbay, which copies the data at start. false
// means the dialog was cancelled.
export type DataFolder = { path: string; default: boolean; pending?: string; notice?: string };
export const dataFolder = () => SettingsService.DataFolder() as Promise<DataFolder>;
export const dataFolderMove = () => SettingsService.DataFolderMove() as Promise<boolean>;

// ---- Terminal background image (KTD41) ----
// Go picks the file in its own dialog and returns a data: URL; the page
// never names a path. "" is no image (or a cancelled dialog).
export const backgroundPick = () => AppearanceService.BackgroundPick();
export const backgroundGet = () => AppearanceService.BackgroundGet();
export const backgroundClear = () => AppearanceService.BackgroundClear();

// ---- Terminal tools (R69–R71, KTD39): logs, post-login rules, history ----
// localEcho: Go shows typed text itself (serial or raw TCP), so history is off.
export type TerminalState = { localEcho: boolean; logging: boolean; logPath?: string };
export const terminalState = (ptyId: string) => TerminalToolsService.TerminalState(ptyId) as Promise<TerminalState>;
export const terminalSetLogging = (ptyId: string, on: boolean) =>
  TerminalToolsService.SetLogging(ptyId, on) as Promise<TerminalState>;
/** A line about the terminal's tools: a rule that stopped, a log that failed. */
export const onTerminalNotice = (ptyId: string, cb: (message: string) => void) =>
  on<string>(`terminal-notice://${ptyId}`, cb);
export const logFolder = () => TerminalToolsService.LogFolder();
/** Asks in a dialog Go opens; "" when cancelled. */
export const chooseLogFolder = () => TerminalToolsService.ChooseLogFolder();
export const resetLogFolder = () => TerminalToolsService.ResetLogFolder();
// kind "" sends reply; "password" the host's saved password; "secret" the
// rule's own secret, which stays in Go: the page sees only hasAnswer.
export type PostLoginStep = { expect?: string; reply?: string; kind?: "" | "password" | "secret" };
export type PostLoginRule = { id: string; hostId: string; steps: PostLoginStep[]; hasAnswer: boolean; held: boolean };
export const postLoginRules = (hostId: string) =>
  list(TerminalToolsService.PostLoginRules(hostId)) as Promise<PostLoginRule[]>;
// answer null keeps the saved secret, "" removes it.
export const savePostLoginRule = (rule: PostLoginRule, answer: string | null) =>
  unlocking(() => TerminalToolsService.SavePostLoginRule(rule, answer) as Promise<PostLoginRule>);
export const deletePostLoginRule = (id: string) => TerminalToolsService.DeletePostLoginRule(id);
export const addHistory = (ptyId: string, line: string) => TerminalToolsService.AddHistory(ptyId, line);
/** The terminal's host's commands, oldest first. */
export const terminalHistory = (ptyId: string) => list(TerminalToolsService.History(ptyId));
export const clearHistory = (ptyId: string) => TerminalToolsService.ClearHistory(ptyId);

// ---- Saved workspaces (R65): host IDs and the layout only (KTD40) ----
// A pane names its host; a split lays its children out in a row or column.
export type WorkspaceNode = { hostId?: string; dir?: string; sizes?: number[] | null; children?: WorkspaceNode[] | null };
export type Workspace = { id: string; name: string; layout: WorkspaceNode };
export const listWorkspaces = () => list(WorkspaceService.ListWorkspaces()) as Promise<Workspace[]>;
export const saveWorkspace = (w: Workspace) => WorkspaceService.SaveWorkspace(w) as Promise<Workspace>;
export const deleteWorkspace = (id: string) => WorkspaceService.DeleteWorkspace(id);

// ---- Updates (R55) ----
// signed is false while builds carry no release key: nothing is checked or
// downloaded automatically. Install only ever runs when the user asks.
export type UpdateStatus = {
  current: string;
  signed: boolean;
  lastCheck: string;
  available: string;
  pageUrl: string;
  canInstall: boolean;
  installed: boolean;
  error: string;
};
export const updateStatus = () => UpdateService.Status() as Promise<UpdateStatus>;
export const checkForUpdate = (prerelease: boolean) => UpdateService.Check(prerelease) as Promise<UpdateStatus>;
export const installUpdate = () => UpdateService.Install() as Promise<UpdateStatus>;

// ---- AI agent (R60, KTD31-KTD34) ----
// The provider key goes to Go once, in aiSaveProvider or aiTestProvider,
// and never comes back: keyState says only whether one is saved. Every
// command the agent proposes runs from Go's own copy after the approval
// tray (or the user's allowlist) lets it.
export type AIProtocol = "anthropic" | "openai-chat" | "claude-code";
// retype: the saved key no longer opens because the protocol, address or
// proxy changed; type it again.
export type AIProvider = {
  id: string;
  name: string;
  protocol: AIProtocol;
  baseUrl: string;
  model: string;
  proxyId: string;
  unencrypted: boolean;
  keyState: "none" | "saved" | "retype";
  /** claude-code only: the program Go chose and whether its path was confirmed. The page cannot set them. */
  execPath: string;
  consented: boolean;
};
// Claude Code login (R1, R5): a provider with no key that runs the user's
// own signed-in Claude Code. Go finds the program, checks it and records the
// consent; the page names a candidate by index and never writes a path.
export type ClaudeCodeCandidate = { index: number; path: string; source: string };
export type ClaudeCodeState = "notFound" | "tooOld" | "signedOut" | "unsupported" | "consent" | "ready";
export type ClaudeCodeStatus = {
  state: ClaudeCodeState;
  message: string;
  path: string;
  target: string;
  version: string;
  authMethod: string;
  consented: boolean;
};
export type AIExecutionMode = "confirm-all" | "dangerous-write" | "dangerous-only";
export type AIMode = { mode: AIExecutionMode; bypass: boolean };
export type AIAllowEntry = { command: string; prefix: boolean };
export type AIAllowGroup = { hostId: string; label: string; entries: AIAllowEntry[] };
export type AITarget = { id: string; label: string; confirmOnly: boolean };
// One request in the app-wide tray; bytes are exactly what runs.
export type AIPending = {
  id: string;
  principal: string;
  client?: string; // an MCP request's token name
  target: string;
  host: string;
  tool: string;
  bytes: string;
  size: number;
  expires: string;
  /** save_skill: the skill's name, with bytes its whole body. */
  name?: string;
  /** save_skill: a skill with this name is saved and would be replaced. */
  replaces?: boolean;
};
export type AIEvent = {
  type: "text" | "command" | "skill" | "result" | "error" | "state" | "stopped" | "targetClosed";
  text?: string;
  toolId?: string;
  command?: string;
  host?: string;
  state?: string;
  retry?: boolean;
  code?: string;
  details?: Record<string, string>;
  /** skill: the proposed skill's name; its body is text. */
  name?: string;
  replaces?: boolean;
};
export const aiListProviders = () => list(AIService.ListProviders()) as Promise<AIProvider[]>;
export const aiDefaultModels = (protocol: AIProtocol) => list(AIService.DefaultModels(protocol));
// apiKey "" keeps the saved key while the provider still points where it did.
export const aiSaveProvider = (p: AIProvider, apiKey: string) =>
  unlocking(() => AIService.SaveProvider(p, apiKey) as Promise<AIProvider>);
export const aiDeleteProvider = (id: string) => AIService.DeleteProvider(id);
/** Lists the provider's models with the typed key, or the saved one. */
export const aiTestProvider = (p: AIProvider, apiKey: string) => unlocking(() => list(AIService.TestProvider(p, apiKey)));
export const aiClaudeCodeCandidates = () => list(AIService.ClaudeCodeCandidates()) as Promise<ClaudeCodeCandidate[]>;
export const aiClaudeCodeSelect = (id: string, index: number) => AIService.ClaudeCodeSelect(id, index);
/** Go's own file dialog; a cancelled one changes nothing. */
export const aiClaudeCodePickFile = (id: string) => AIService.ClaudeCodePickFile(id);
/** Runs the chosen program's --version and auth status; no model call. */
export const aiClaudeCodeState = (id: string) => AIService.ClaudeCodeState(id) as Promise<ClaudeCodeStatus>;
/** Opens a Go dialog that states what is sent and records the answer. */
export const aiClaudeCodeConsent = (id: string) => AIService.ClaudeCodeConsent(id);
export const aiTrustCert = (requestId: string, accept: boolean) => AIService.TrustCert(requestId, accept);
export const aiGetMode = () => AIService.GetMode() as Promise<AIMode>;
// A wider mode, bypass or a new entry opens a Go dialog, then OS verification.
export const aiSetMode = (mode: AIExecutionMode) => AIService.SetMode(mode);
export const aiSetBypass = (on: boolean) => AIService.SetBypass(on);
export const aiListAllowlist = () => list(AIService.ListAllowlist()) as Promise<AIAllowGroup[]>;
export const aiAddAllowEntry = (hostId: string, command: string, prefix: boolean) => AIService.AddAllowEntry(hostId, command, prefix);
export const aiRemoveAllowEntry = (hostId: string, command: string, prefix: boolean) =>
  AIService.RemoveAllowEntry(hostId, command, prefix);
export const aiTerminals = () => list(AIService.Terminals()) as Promise<AITarget[]>;
export const aiSetActiveTerminal = (ptyId: string) => AIService.SetActiveTerminal(ptyId);
/** ptyId "" follows the focused terminal. */
export const aiStartConversation = (providerId: string, ptyId: string) => AIService.StartConversation(providerId, ptyId);
export const aiSend = (convId: string, text: string) => unlocking(() => AIService.Send(convId, text));
export const aiRetry = (convId: string) => AIService.Retry(convId);
export const aiStop = (convId: string) => AIService.Stop(convId);
export const aiCloseConversation = (convId: string) => AIService.CloseConversation(convId);
export const aiJobDone = (convId: string) => AIService.JobDone(convId);
export const aiApprovals = () => list(AIService.Approvals()) as Promise<AIPending[]>;
export const aiAnswerApproval = (requestId: string, allow: boolean) => AIService.AnswerApproval(requestId, allow);
export const onAIEvent = (convId: string, cb: (e: AIEvent) => void) => on<AIEvent>(`ai://${convId}`, cb);
// The whole tray, oldest first, whenever it changes; Go sends null for none.
export const onAIApprovals = (cb: (l: AIPending[]) => void) =>
  on<AIPending[] | null>("ai-approvals", (l) => cb(l ?? []));

// Skills and prompt templates (R61). A skill's body joins a request only
// when the user types /name; a template becomes the message.
export type AISkillKind = "skill" | "template";
export type AISkill = { id: string; name: string; description: string; body: string; kind: AISkillKind; agentAuthored: boolean };
export const aiListSkills = () => list(AIService.ListSkills()) as Promise<AISkill[]>;
export const aiSaveSkill = (s: AISkill) => AIService.SaveSkill(s) as Promise<AISkill>;
export const aiDeleteSkill = (id: string) => AIService.DeleteSkill(id);
/** The AI-completed line, or "" (R62). Go refuses while suggestions are off. */
export const aiSuggest = (ptyId: string, line: string) => AIService.Suggest(ptyId, line);
/** A question as SQL for the editor (R64). It runs nothing. */
export const aiAskSql = (databaseId: string, question: string) => unlocking(() => AIService.AskSQL(databaseId, question));

// Settings > MCP (R63, KTD35). No call returns a token: Go copies a new one
// to the clipboard, and the page sees only names and dates.
export type MCPStatus = { enabled: boolean; running: boolean; port: number; endpoint: string; error?: string; folder: string };
export type MCPToken = { id: string; name: string; created: number; lastUsed?: number };
export type MCPNewToken = { token: MCPToken; copied: boolean; config: string; claudeCode: string };
export type MCPAllowEntry = { tokenId: string; tokenName: string; hostId: string; hostLabel: string; command: string };
export const mcpStatus = () => MCPService.Status() as Promise<MCPStatus>;
// On asks in a dialog Go opens, then OS verification.
export const mcpSetEnabled = (on: boolean) => MCPService.SetEnabled(on);
export const mcpSetPort = (port: number) => MCPService.SetPort(port);
export const mcpListTokens = () => list(MCPService.ListTokens()) as Promise<MCPToken[]>;
export const mcpCreateToken = (name: string) => MCPService.CreateToken(name) as Promise<MCPNewToken>;
export const mcpRevokeToken = (id: string) => MCPService.RevokeToken(id);
export const mcpListAllowlist = () => list(MCPService.ListAllowlist()) as Promise<MCPAllowEntry[]>;
export const mcpAddAllowEntry = (tokenId: string, hostId: string, command: string) => MCPService.AddAllowEntry(tokenId, hostId, command);
export const mcpRemoveAllowEntry = (tokenId: string, hostId: string, command: string) => MCPService.RemoveAllowEntry(tokenId, hostId, command);
export type { MCPVPNAllow } from "./bindings/hostbay/internal/app/models";
export const mcpListVPNAllow = () => MCPService.ListVPNAllow();
// Asks in a dialog Go opens, then OS verification; Go refuses system-wide profiles.
export const mcpAddVPNAllow = (tokenId: string, vpnId: string) => MCPService.AddVPNAllow(tokenId, vpnId);
export const mcpRemoveVPNAllow = (tokenId: string, vpnId: string) => MCPService.RemoveVPNAllow(tokenId, vpnId);
// The folder is picked in a dialog Go opens; "" means cancelled.
export const mcpPickFolder = () => MCPService.PickFolder();
