//! One RDP session via IronRDP's blocking API: TCP → connect_begin → TLS
//! upgrade → certificate check by Hostbay → connect_finalize (CredSSP/NLA) →
//! ActiveStage loop. Changed rectangles go to Hostbay as raw RGBA frames on
//! stdout; input, clipboard and close arrive as `protocol::In` messages.
//!
//! The TLS verifier accepts any certificate on purpose (RDP servers use
//! self-signed ones): Hostbay pins the certificate instead, and no
//! credential is sent until it answers `trust` (KTD3, R28).
//!
//! NOTE: unverified against a live Windows host from the dev machine — the
//! connect/auth path (CredSSP) is the most likely place to need adjustment.

use crate::protocol::{In, Out, Sink};
use base64::Engine;
use ironrdp::cliprdr::backend::CliprdrBackend;
use ironrdp::cliprdr::pdu::{
    ClipboardFileAttributes, ClipboardFormat, ClipboardFormatId,
    ClipboardGeneralCapabilityFlags, FileContentsFlags, FileContentsRequest, FileContentsResponse,
    FileDescriptor, FormatDataRequest, FormatDataResponse, OwnedFormatDataResponse, PackedFileList,
};
use ironrdp::cliprdr::{Cliprdr, CliprdrClient};
use ironrdp::connector::{self, Credentials};
use ironrdp::graphics::image_processing::PixelFormat;
use ironrdp::input::{Database, MouseButton, MousePosition, Operation, Scancode, WheelRotations};
use ironrdp::pdu::gcc::{ConnectionType, KeyboardType};
use ironrdp::pdu::geometry::InclusiveRectangle;
use ironrdp::session::image::DecodedImage;
use ironrdp::session::{ActiveStageBuilder, ActiveStageOutput};
use std::collections::HashMap;
use std::io::Write;
use std::net::TcpStream;
use std::sync::mpsc::{Receiver, Sender};
use std::sync::{Arc, Mutex};
use std::time::Duration;

type AppResult<T> = Result<T, String>;

const CF_UNICODETEXT: u16 = 13;

// ---- Clipboard (cliprdr) ----
//
// Bidirectional sync for TEXT and FILES. The CliprdrBackend runs inside the
// Cliprdr SVC processor (on the RDP thread). Its callbacks push ClipIntents
// into a shared outbox which the active loop drains and turns into the right
// Cliprdr method call (initiate_copy / initiate_file_copy / initiate_paste /
// submit_format_data / request_file_contents / submit_file_contents), writing
// the resulting PDUs to the socket.
//
// Text flows over CF_UNICODETEXT via `clipboardText` messages both ways.
// Files flow over the FileGroupDescriptorW format + FileContents
// streaming, using the native macOS pasteboard (see pasteboard.rs) since the
// webview clipboard can't hold file references.

const FILE_LIST_NAME: &str = "FileGroupDescriptorW";
/// Chunk read from a local file per RANGE request when the server doesn't ask
/// for a specific size.
const UPLOAD_CHUNK: u32 = 4 * 1024 * 1024;

/// One local file staged for upload: its wire descriptor + absolute source path.
struct LocalFile {
    desc: FileDescriptor,
    path: std::path::PathBuf,
}

/// A remote file being downloaded: where its bytes are written, how many are
/// still outstanding, and how many have been written so far (the read offset).
struct DownloadFile {
    file: std::fs::File,
    remaining: u64,
    written: u64,
}

/// What each outstanding FileContentsRequest we sent is fetching.
struct DlStream {
    index: usize,
}

/// An action the backend wants the active loop to perform on the Cliprdr.
enum ClipIntent {
    /// Advertise local text (CF_UNICODETEXT) for the remote to paste.
    CopyText,
    /// Advertise local files (FileGroupDescriptorW) for the remote to paste.
    CopyFiles(Vec<FileDescriptor>),
    /// Ask the remote for its clipboard in the given format (paste).
    Paste(ClipboardFormatId),
    /// Serve a format-data response (text or file list) to the remote.
    FormatData(OwnedFormatDataResponse),
    /// Ask the remote for a chunk of one of its files.
    FileRequest(FileContentsRequest),
    /// Serve a chunk of one of our local files to the remote.
    FileResponse(FileContentsResponse<'static>),
}

impl ClipIntent {
    fn label(&self) -> &'static str {
        match self {
            ClipIntent::CopyText => "CopyText",
            ClipIntent::CopyFiles(_) => "CopyFiles",
            ClipIntent::Paste(_) => "Paste",
            ClipIntent::FormatData(_) => "FormatData",
            ClipIntent::FileRequest(_) => "FileRequest",
            ClipIntent::FileResponse(_) => "FileResponse",
        }
    }
}

#[derive(Default)]
struct ClipShared {
    /// Actions the backend wants performed on the channel, in order.
    outbox: Vec<ClipIntent>,
    /// Text we've copied locally (Mac) — served when the remote requests it.
    local_text_utf16le: Option<Vec<u8>>,

    /// Files we've copied locally (Mac), flattened; index == server lindex.
    local_files: Vec<LocalFile>,
    /// Format id the remote uses for FileGroupDescriptorW (learned on copy).
    remote_file_format: Option<ClipboardFormatId>,
    /// The Cliprdr channel has reached Ready state. `initiate_file_copy` is
    /// rejected before this; text `initiate_copy` bootstraps it. We hold file
    /// copies until Ready, then flush them.
    ready: bool,

    /// Download scratch dir for the current remote->Mac transfer.
    dl_dir: Option<std::path::PathBuf>,
    /// Open write handles for the files currently being downloaded, by index.
    dl_files: HashMap<usize, DownloadFile>,
    /// Outstanding FileContentsRequests we sent, by stream id.
    dl_streams: HashMap<u32, DlStream>,
    /// Next stream id to hand out.
    next_stream: u32,
    /// How many download files are not yet fully written.
    dl_pending: usize,
    /// changeCount of the last file list WE wrote to the Mac pasteboard — the
    /// poll skips it so a remote->Mac copy isn't echoed back to the remote.
    self_written_change: isize,
}

struct AppClip {
    out: Sink,
    shared: Arc<Mutex<ClipShared>>,
}

// AppClip must be Debug (CliprdrBackend bound); a name is enough.
impl core::fmt::Debug for AppClip {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        write!(f, "AppClip")
    }
}

fn text_to_utf16le(s: &str) -> Vec<u8> {
    let mut v: Vec<u8> = s.encode_utf16().flat_map(|u| u.to_le_bytes()).collect();
    v.extend_from_slice(&[0, 0]); // NUL terminator
    v
}

/// Flatten a set of Mac paths (files and/or directories) into wire
/// FileDescriptors + their absolute source paths. Directories recurse; each
/// child carries a `relative_path` rooted at the dropped item's name.
fn flatten_local(paths: &[String]) -> Vec<LocalFile> {
    fn walk(abs: &std::path::Path, rel: Option<String>, out: &mut Vec<LocalFile>) {
        let name = abs.file_name().map(|n| n.to_string_lossy().into_owned());
        let Some(name) = name else { return };
        let meta = match std::fs::metadata(abs) {
            Ok(m) => m,
            Err(_) => return,
        };
        let is_dir = meta.is_dir();
        let attrs = if is_dir {
            ClipboardFileAttributes::DIRECTORY
        } else {
            ClipboardFileAttributes::ARCHIVE
        };
        let mut desc = FileDescriptor::new(name.clone())
            .with_attributes(attrs)
            .with_file_size(if is_dir { 0 } else { meta.len() });
        if let Some(r) = &rel {
            desc = desc.with_relative_path(r.clone());
        }
        out.push(LocalFile { desc, path: abs.to_path_buf() });
        if is_dir {
            // Children's relative_path is parent's path + this dir name.
            let child_rel = match &rel {
                Some(r) => format!("{r}\\{name}"),
                None => name.clone(),
            };
            if let Ok(entries) = std::fs::read_dir(abs) {
                for e in entries.flatten() {
                    walk(&e.path(), Some(child_rel.clone()), out);
                }
            }
        }
    }
    let mut out = Vec::new();
    for p in paths {
        walk(std::path::Path::new(p), None, &mut out);
    }
    out
}

/// Reconstruct the on-disk relative path for a downloaded descriptor, under
/// `dir`. Windows `\` separators become the local `/`. Names come from the
/// server, so a segment that could climb out of `dir` (`..`, a separator,
/// a drive letter) is dropped, and such a file name becomes `_`.
fn dl_target(dir: &std::path::Path, d: &FileDescriptor) -> std::path::PathBuf {
    fn safe(seg: &str) -> bool {
        !seg.is_empty() && seg != "." && seg != ".." && !seg.contains(['/', '\\', ':'])
    }
    let mut p = dir.to_path_buf();
    if let Some(rel) = &d.relative_path {
        for seg in rel.split('\\').filter(|s| safe(s)) {
            p.push(seg);
        }
    }
    p.push(if safe(&d.name) { d.name.as_str() } else { "_" });
    p
}

fn is_dir_desc(d: &FileDescriptor) -> bool {
    d.attributes
        .map(|a| a.contains(ClipboardFileAttributes::DIRECTORY))
        .unwrap_or(false)
}

impl AppClip {
    fn push(&self, intent: ClipIntent) {
        self.shared.lock().unwrap().outbox.push(intent);
    }
}

impl CliprdrBackend for AppClip {
    fn temporary_directory(&self) -> &str {
        "."
    }
    fn client_capabilities(&self) -> ClipboardGeneralCapabilityFlags {
        // Long format names + file-clip streaming (needed for file transfer).
        ClipboardGeneralCapabilityFlags::USE_LONG_FORMAT_NAMES
            | ClipboardGeneralCapabilityFlags::STREAM_FILECLIP_ENABLED
    }
    fn on_ready(&mut self) {
        // Channel is now Ready — `initiate_file_copy` is allowed. Flush any file
        // selection that was staged during initialization (it couldn't be sent
        // before, since file copy requires Ready but text copy bootstraps it).
        let files: Vec<FileDescriptor> = {
            let mut s = self.shared.lock().unwrap();
            s.ready = true;
            s.local_files.iter().map(|f| f.desc.clone()).collect()
        };
        if !files.is_empty() {
            eprintln!("[rdp-clip] channel Ready — flushing {} staged file(s)", files.len());
            self.push(ClipIntent::CopyFiles(files));
        }
    }
    fn on_request_format_list(&mut self) {
        // Fires at Monitor Ready (before the channel is Ready). We MUST send an
        // initiate_copy here to complete the FormatList handshake and reach
        // Ready — file copy can't do that (it requires Ready). So advertise
        // text now; staged files are flushed from on_ready() once Ready.
        self.push(ClipIntent::CopyText);
    }
    fn on_process_negotiated_capabilities(&mut self, c: ClipboardGeneralCapabilityFlags) {
        eprintln!(
            "[rdp-clip] negotiated caps: file_stream={}",
            c.contains(ClipboardGeneralCapabilityFlags::STREAM_FILECLIP_ENABLED)
        );
    }

    fn on_remote_copy(&mut self, available_formats: &[ClipboardFormat]) {
        // The remote copied something. Prefer files, else text.
        let file_fmt = available_formats.iter().find(|f| {
            f.name().map(|n| n.value() == FILE_LIST_NAME).unwrap_or(false)
        });
        if let Some(f) = file_fmt {
            eprintln!("[rdp-clip] remote copied FILES (fmt {:?})", f.id());
            self.shared.lock().unwrap().remote_file_format = Some(f.id());
            self.push(ClipIntent::Paste(f.id()));
            return;
        }
        let has_text = available_formats
            .iter()
            .any(|f| f.id().value() == CF_UNICODETEXT as u32);
        if has_text {
            // Remote copied text, not files — clear any stale file-format id so
            // the response isn't mis-parsed as a file list.
            self.shared.lock().unwrap().remote_file_format = None;
            self.push(ClipIntent::Paste(ClipboardFormatId::new(CF_UNICODETEXT as u32)));
        }
    }

    fn on_format_data_request(&mut self, request: FormatDataRequest) {
        // Remote is pasting from us. Serve files if it asked for the file list,
        // else serve staged text.
        let want_files = {
            let s = self.shared.lock().unwrap();
            !s.local_files.is_empty()
                && (Some(request.format) == s.remote_file_format
                    || s.local_text_utf16le.is_none())
        };
        if want_files {
            let list = PackedFileList {
                files: self
                    .shared
                    .lock()
                    .unwrap()
                    .local_files
                    .iter()
                    .map(|f| f.desc.clone())
                    .collect(),
            };
            match OwnedFormatDataResponse::new_file_list(&list) {
                Ok(resp) => self.push(ClipIntent::FormatData(resp)),
                Err(_) => self.push(ClipIntent::FormatData(OwnedFormatDataResponse::new_error())),
            }
            return;
        }
        let data = self.shared.lock().unwrap().local_text_utf16le.clone();
        let resp = match data {
            Some(bytes) => OwnedFormatDataResponse::new_data(bytes),
            None => OwnedFormatDataResponse::new_data(Vec::new()),
        };
        self.push(ClipIntent::FormatData(resp));
    }

    fn on_format_data_response(&mut self, response: FormatDataResponse<'_>) {
        eprintln!("[rdp-clip] >> on_format_data_response fired");
        if response.is_error() {
            eprintln!("[rdp-clip] format_data_response: ERROR from remote");
            return;
        }
        // Is this the file list we requested? Try to decode it as one.
        let is_file_paste = self.shared.lock().unwrap().remote_file_format.is_some();
        eprintln!(
            "[rdp-clip] format_data_response: {} bytes, file_paste={}",
            response.data().len(),
            is_file_paste
        );
        if is_file_paste {
            match response.to_file_list() {
                Ok(list) => {
                    eprintln!("[rdp-clip] decoded file list: {} file(s)", list.files.len());
                    self.begin_download(list.files);
                    return;
                }
                Err(e) => eprintln!("[rdp-clip] file list decode FAILED: {e}"),
            }
        }
        // Otherwise it's text — write it straight to the Mac clipboard
        // natively (the webview navigator.clipboard is unreliable/permissioned).
        if let Ok(text) = response.to_unicode_string() {
            // Record the changeCount we're about to cause so the poll thread
            // doesn't bounce this text back to the remote.
            crate::pasteboard::write_text(&text);
            self.shared.lock().unwrap().self_written_change = crate::pasteboard::change_count();
            self.out.send(&Out::ClipboardText { text });
        }
    }

    fn on_file_contents_request(&mut self, request: FileContentsRequest) {
        // Remote wants a chunk of one of our local files.
        let idx = request.index as usize;
        let (path, is_dir) = {
            let s = self.shared.lock().unwrap();
            match s.local_files.get(idx) {
                Some(lf) => (lf.path.clone(), is_dir_desc(&lf.desc)),
                None => {
                    drop(s);
                    self.push(ClipIntent::FileResponse(FileContentsResponse::new_error(
                        request.stream_id,
                    )));
                    return;
                }
            }
        };
        if request.flags.contains(FileContentsFlags::SIZE) {
            let size = if is_dir {
                0
            } else {
                std::fs::metadata(&path).map(|m| m.len()).unwrap_or(0)
            };
            self.push(ClipIntent::FileResponse(FileContentsResponse::new_size_response(
                request.stream_id,
                size,
            )));
            return;
        }
        // RANGE request: read the asked-for slice.
        // ponytail: re-opens + seeks per chunk — O(chunks) opens. Fine for
        // typical sizes; keep a handle cache if huge files get slow.
        let want = if request.requested_size == 0 {
            UPLOAD_CHUNK
        } else {
            request.requested_size
        };
        let data = read_slice(&path, request.position, want as usize);
        match data {
            Some(bytes) => self.push(ClipIntent::FileResponse(
                FileContentsResponse::new_data_response(request.stream_id, bytes),
            )),
            None => self.push(ClipIntent::FileResponse(FileContentsResponse::new_error(
                request.stream_id,
            ))),
        }
    }

    fn on_file_contents_response(&mut self, response: FileContentsResponse<'_>) {
        use std::io::Write as _;
        let stream_id = response.stream_id();
        eprintln!(
            "[rdp-clip] file_contents_response: stream={} err={} {} bytes",
            stream_id,
            response.is_error(),
            response.data().len()
        );
        let index = {
            let mut s = self.shared.lock().unwrap();
            match s.dl_streams.remove(&stream_id) {
                Some(st) => st.index,
                None => {
                    eprintln!("[rdp-clip]   unknown stream {stream_id} — dropped");
                    return;
                }
            }
        };
        if response.is_error() {
            self.finish_download_file(index);
            return;
        }
        let data = response.data().to_vec();
        let (done, all_done) = {
            let mut s = self.shared.lock().unwrap();
            let Some(df) = s.dl_files.get_mut(&index) else {
                return;
            };
            let _ = df.file.write_all(&data);
            df.written += data.len() as u64;
            df.remaining = df.remaining.saturating_sub(data.len() as u64);
            let done = df.remaining == 0;
            (done, done && s.dl_pending <= 1)
        };
        if done {
            self.finish_download_file(index);
        }
        if all_done {
            self.complete_download();
        } else if !done {
            // Request the next chunk of this file.
            self.request_next_chunk(index);
        }
    }

    fn on_lock(&mut self, _id: ironrdp::cliprdr::pdu::LockDataId) {}
    fn on_unlock(&mut self, _id: ironrdp::cliprdr::pdu::LockDataId) {}
}

impl AppClip {
    /// Remote sent its file list — create the scratch dir, make directories,
    /// open files, and kick off the first chunk request for each file.
    fn begin_download(&self, files: Vec<FileDescriptor>) {
        let dir = std::env::temp_dir().join(format!("hostbay-rdp-clip-{}", std::process::id()));
        eprintln!("[rdp-clip] begin_download: {} file(s) -> {}", files.len(), dir.display());
        let _ = std::fs::remove_dir_all(&dir);
        let _ = std::fs::create_dir_all(&dir);

        let mut roots: Vec<std::path::PathBuf> = Vec::new();
        {
            let mut s = self.shared.lock().unwrap();
            s.dl_dir = Some(dir.clone());
            s.dl_files.clear();
            s.dl_streams.clear();
            s.dl_pending = 0;

            for (idx, d) in files.iter().enumerate() {
                let target = dl_target(&dir, d);
                if d.relative_path.is_none() {
                    roots.push(target.clone());
                }
                if is_dir_desc(d) {
                    let _ = std::fs::create_dir_all(&target);
                    continue;
                }
                if let Some(parent) = target.parent() {
                    let _ = std::fs::create_dir_all(parent);
                }
                match std::fs::File::create(&target) {
                    Ok(file) => {
                        let remaining = d.file_size.unwrap_or(0);
                        s.dl_files
                            .insert(idx, DownloadFile { file, remaining, written: 0 });
                        s.dl_pending += 1;
                    }
                    Err(_) => {}
                }
            }
        }
        // Empty files (remaining==0) complete immediately; others fetch a chunk.
        let indices: Vec<usize> = self.shared.lock().unwrap().dl_files.keys().copied().collect();
        let mut any_pending = false;
        for idx in indices {
            let empty = {
                let s = self.shared.lock().unwrap();
                s.dl_files.get(&idx).map(|d| d.remaining == 0).unwrap_or(true)
            };
            if empty {
                self.finish_download_file(idx);
            } else {
                any_pending = true;
                self.request_next_chunk(idx);
            }
        }
        if !any_pending {
            // Only dirs / empty files — finalize now with the root paths.
            self.write_roots(&roots);
        } else {
            // Stash roots for completion (store on shared via dl_dir already; we
            // recompute roots at completion from dl_dir, so nothing to keep).
        }
    }

    fn request_next_chunk(&self, index: usize) {
        let (stream_id, position, size) = {
            let mut s = self.shared.lock().unwrap();
            let Some(df) = s.dl_files.get(&index) else { return };
            let position = df.written;
            // The server rejects a RANGE request whose end exceeds the file
            // size, so cap the chunk to the bytes that actually remain.
            let size = df.remaining.min(UPLOAD_CHUNK as u64) as u32;
            let sid = s.next_stream;
            s.next_stream = s.next_stream.wrapping_add(1);
            s.dl_streams.insert(sid, DlStream { index });
            (sid, position, size)
        };
        if size == 0 {
            self.finish_download_file(index);
            return;
        }
        let req = FileContentsRequest {
            stream_id,
            index: index as i32,
            flags: FileContentsFlags::RANGE,
            position,
            requested_size: size,
            data_id: None,
        };
        self.push(ClipIntent::FileRequest(req));
    }

    fn finish_download_file(&self, index: usize) {
        let mut s = self.shared.lock().unwrap();
        if s.dl_files.remove(&index).is_some() && s.dl_pending > 0 {
            s.dl_pending -= 1;
        }
    }

    fn complete_download(&self) {
        let dir = self.shared.lock().unwrap().dl_dir.clone();
        let Some(dir) = dir else { return };
        // Roots = immediate children of the scratch dir.
        let mut roots = Vec::new();
        if let Ok(entries) = std::fs::read_dir(&dir) {
            for e in entries.flatten() {
                roots.push(e.path());
            }
        }
        self.write_roots(&roots);
    }

    fn write_roots(&self, roots: &[std::path::PathBuf]) {
        let paths: Vec<String> = roots
            .iter()
            .map(|p| p.to_string_lossy().into_owned())
            .collect();
        eprintln!("[rdp-clip] write_roots: {} path(s) -> Mac clipboard", paths.len());
        if !paths.is_empty() {
            crate::pasteboard::write_files(&paths);
            // Record the resulting changeCount so the poll thread doesn't echo
            // this selection straight back to the remote.
            self.shared.lock().unwrap().self_written_change = crate::pasteboard::change_count();
            self.out.send(&Out::ClipboardFiles { paths });
        }
    }
}

/// Read `len` bytes from `path` starting at `pos`. Returns fewer bytes at EOF.
fn read_slice(path: &std::path::Path, pos: u64, len: usize) -> Option<Vec<u8>> {
    use std::io::{Read as _, Seek as _, SeekFrom};
    let mut f = std::fs::File::open(path).ok()?;
    f.seek(SeekFrom::Start(pos)).ok()?;
    let mut buf = vec![0u8; len];
    let mut filled = 0;
    while filled < len {
        match f.read(&mut buf[filled..]) {
            Ok(0) => break,
            Ok(n) => filled += n,
            Err(_) => break,
        }
    }
    buf.truncate(filled);
    Some(buf)
}

impl ironrdp::core::AsAny for AppClip {
    fn as_any(&self) -> &dyn core::any::Any {
        self
    }
    fn as_any_mut(&mut self) -> &mut dyn core::any::Any {
        self
    }
}

fn build_config(
    username: String,
    password: String,
    domain: Option<String>,
    width: u16,
    height: u16,
    security: &str,
) -> connector::Config {
    use ironrdp::pdu::rdp::capability_sets::MajorPlatformType;
    // "tls" = TLS without CredSSP/NLA (xrdp / Linux hosts that don't do NLA).
    // "nla" = force CredSSP: only HYBRID(_EX) is offered, so the server
    // cannot downgrade to plain TLS. "auto" (default) = TLS + CredSSP.
    let enable_credssp = security != "tls";
    connector::Config {
        credentials: Credentials::UsernamePassword { username, password },
        domain,
        enable_tls: security != "nla",
        enable_credssp,
        enable_standard_rdp_security: false,
        keyboard_type: KeyboardType::IBM_ENHANCED,
        keyboard_subtype: 0,
        keyboard_layout: 0,
        keyboard_functional_keys_count: 12,
        ime_file_name: String::new(),
        dig_product_id: String::new(),
        desktop_size: connector::DesktopSize { width, height },
        monitor_layout: None,
        // LAN is what the released connector hardcoded before it became a field.
        connection_type: ConnectionType::Lan,
        bitmap: None,
        client_build: 0,
        client_name: "hostbay".to_owned(),
        client_dir: "C:\\Windows\\System32\\mstscax.dll".to_owned(),
        #[cfg(target_os = "macos")]
        platform: MajorPlatformType::MACINTOSH,
        #[cfg(target_os = "windows")]
        platform: MajorPlatformType::WINDOWS,
        #[cfg(target_os = "linux")]
        platform: MajorPlatformType::UNIX,
        // Server pointer OFF: if the server composites the cursor into the
        // framebuffer, our changed-rect emit doesn't repaint where the cursor
        // LEFT, leaving a trail. Instead we show the local OS cursor (canvas
        // cursor:default) which tracks the mouse 1:1 since we forward moves.
        enable_server_pointer: false,
        request_data: None,
        autologon: false,
        enable_audio_playback: true,
        // Matches the released connector, which never set INFO_AUDIOCAPTURE.
        enable_audio_capture: false,
        compression_type: None,
        // Don't render the pointer into the framebuffer (avoids cursor trails
        // with changed-rect emit). Local OS cursor is shown instead.
        pointer_software_rendering: false,
        multitransport_flags: None,
        performance_flags: Default::default(),
        desktop_scale_factor: 0,
        hardware_id: None,
        license_cache: None,
        timezone_info: Default::default(),
        alternate_shell: String::new(),
        work_dir: String::new(),
        remote_application_mode: false,
        rail_support_level: ironrdp::pdu::rdp::capability_sets::RailSupportLevel::empty(),
        support_dyn_vc_gfx_protocol: false,
    }
}

/// TLS upgrade with certificate verification left to Hostbay (see the module
/// doc). Returns the stream, the server public key CredSSP binds to, and the
/// leaf certificate's DER for Hostbay's pin check.
fn tls_upgrade(
    stream: TcpStream,
    server_name: &str,
) -> AppResult<(
    rustls::StreamOwned<rustls::ClientConnection, TcpStream>,
    Vec<u8>,
    Vec<u8>,
)> {
    let mut config = rustls::client::ClientConfig::builder()
        .dangerous()
        .with_custom_certificate_verifier(std::sync::Arc::new(danger::NoChainVerify::new()))
        .with_no_client_auth();
    config.resumption = rustls::client::Resumption::disabled();
    let config = std::sync::Arc::new(config);

    let sni = server_name
        .to_string()
        .try_into()
        .map_err(|_| String::from("invalid server name for TLS"))?;
    let client = rustls::ClientConnection::new(config, sni)
        .map_err(|e| String::from(format!("tls client: {e}")))?;
    let mut tls = rustls::StreamOwned::new(client, stream);
    tls.flush().map_err(|e| String::from(format!("tls flush: {e}")))?;

    let cert = tls
        .conn
        .peer_certificates()
        .and_then(|c| c.first())
        .ok_or_else(|| String::from("no peer certificate"))?;
    let pubkey = extract_pubkey(cert.as_ref())?;
    let der = cert.as_ref().to_vec();
    Ok((tls, pubkey, der))
}

fn extract_pubkey(cert: &[u8]) -> AppResult<Vec<u8>> {
    use x509_cert::der::Decode as _;
    let cert = x509_cert::Certificate::from_der(cert)
        .map_err(|e| String::from(format!("parse cert: {e}")))?;
    let key = cert
        .tbs_certificate
        .subject_public_key_info
        .subject_public_key
        .as_bytes()
        .ok_or_else(|| String::from("public key not aligned"))?
        .to_owned();
    Ok(key)
}

mod danger {
    use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
    use rustls::crypto::WebPkiSupportedAlgorithms;
    use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
    use rustls::{DigitallySignedStruct, SignatureScheme};

    /// Accepts any certificate chain (Hostbay pins the leaf), but still checks
    /// that the handshake is signed by that leaf's key: otherwise a MITM could
    /// replay the pinned public certificate and sign with its own key.
    #[derive(Debug)]
    pub struct NoChainVerify(WebPkiSupportedAlgorithms);

    impl NoChainVerify {
        pub fn new() -> Self {
            Self(rustls::crypto::ring::default_provider().signature_verification_algorithms)
        }
    }

    impl ServerCertVerifier for NoChainVerify {
        fn verify_server_cert(
            &self,
            _e: &CertificateDer<'_>,
            _i: &[CertificateDer<'_>],
            _s: &ServerName<'_>,
            _o: &[u8],
            _n: UnixTime,
        ) -> Result<ServerCertVerified, rustls::Error> {
            Ok(ServerCertVerified::assertion())
        }
        fn verify_tls12_signature(
            &self,
            m: &[u8],
            c: &CertificateDer<'_>,
            d: &DigitallySignedStruct,
        ) -> Result<HandshakeSignatureValid, rustls::Error> {
            rustls::crypto::verify_tls12_signature(m, c, d, &self.0)
        }
        fn verify_tls13_signature(
            &self,
            m: &[u8],
            c: &CertificateDer<'_>,
            d: &DigitallySignedStruct,
        ) -> Result<HandshakeSignatureValid, rustls::Error> {
            rustls::crypto::verify_tls13_signature(m, c, d, &self.0)
        }
        fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
            self.0.supported_schemes()
        }
    }
}

type UpgradedFramed =
    ironrdp_blocking::Framed<rustls::StreamOwned<rustls::ClientConnection, TcpStream>>;

/// Where `connect` dials and whom it talks to.
pub struct Endpoint {
    /// Where to open TCP: the server, or a local forward through a jump chain.
    pub dial_host: String,
    pub dial_port: u16,
    /// The real server name, for TLS SNI and CredSSP.
    pub target_host: String,
}

/// Wait for Hostbay's answer to a `cert` message. Input and clipboard that
/// arrive meanwhile are dropped: the session has no desktop yet.
fn await_trust(rx: &Receiver<In>) -> AppResult<()> {
    loop {
        match rx.recv() {
            Ok(In::Trust { accept: true }) => return Ok(()),
            Ok(In::Trust { accept: false }) => return Err("the server certificate was not trusted".into()),
            Ok(In::Close) | Err(_) => return Err("closed before the certificate was trusted".into()),
            Ok(_) => {}
        }
    }
}

/// Full connect: TCP → begin → TLS → certificate trust → finalize (CredSSP).
fn connect(
    out: &Sink,
    rx: &Receiver<In>,
    ep: &Endpoint,
    cfg: connector::Config,
) -> AppResult<(
    connector::ConnectionResult,
    UpgradedFramed,
    Arc<Mutex<ClipShared>>,
    crate::audin::Mic,
)> {
    use std::net::ToSocketAddrs;
    let addr = (ep.dial_host.as_str(), ep.dial_port)
        .to_socket_addrs()
        .map_err(|e| format!("resolve {}: {e}", ep.dial_host))?
        .next()
        .ok_or_else(|| String::from("no address"))?;
    let tcp = TcpStream::connect(addr).map_err(|e| format!("tcp connect: {e}"))?;
    // IMPORTANT: NO read timeout during connect. CredSSP round-trips take
    // longer than any short timeout; a short timeout here makes
    // `read_by_hint` fail with "read frame by hint" during auth. We set the
    // active-loop timeout only AFTER finalize, via a cloned handle.
    let sock_handle = tcp.try_clone().ok();
    let client_addr = tcp.local_addr().map_err(|e| e.to_string())?;

    let mut framed = ironrdp_blocking::Framed::new(tcp);
    // Register the clipboard virtual channel (bidirectional text sync).
    let shared: Arc<Mutex<ClipShared>> = Arc::new(Mutex::new(ClipShared::default()));
    let backend = AppClip { out: out.clone(), shared: Arc::clone(&shared) };
    // Mic input rides the AUDIO_INPUT dynamic virtual channel (over drdynvc).
    let mic: crate::audin::Mic = Arc::new(Mutex::new(crate::audin::MicShared::default()));
    let drdynvc = ironrdp::dvc::DrdynvcClient::new()
        .with_dynamic_channel(crate::audin::AudinChannel::new(Arc::clone(&mic)));
    let mut connector = connector::ClientConnector::new(cfg, client_addr)
        .with_static_channel(Cliprdr::new(Box::new(backend)))
        // Remote speaker output (rdpsnd).
        .with_static_channel(ironrdp::rdpsnd::client::Rdpsnd::new(Box::new(
            crate::audio::AudioOut::new(),
        )))
        // Dynamic channels (mic input via AUDIO_INPUT).
        .with_static_channel(drdynvc);
    let should_upgrade = ironrdp_blocking::connect_begin(&mut framed, &mut connector)
        .map_err(|e| format!("connect begin: {e}"))?;

    let initial = framed.into_inner_no_leftover();
    let (upgraded_stream, server_public_key, cert_der) = tls_upgrade(initial, &ep.target_host)?;

    // R28: no credential leaves before Hostbay has pinned or accepted this
    // certificate. connect_finalize below is the first step that sends one.
    out.send(&Out::Cert { der: base64::engine::general_purpose::STANDARD.encode(&cert_der) });
    await_trust(rx)?;

    let upgraded = ironrdp_blocking::mark_as_upgraded(should_upgrade, &mut connector);
    let mut upgraded_framed = ironrdp_blocking::Framed::new(upgraded_stream);

    let mut net = sspi::network_client::reqwest_network_client::ReqwestNetworkClient;
    let result = ironrdp_blocking::connect_finalize(
        upgraded,
        connector,
        &mut upgraded_framed,
        &mut net,
        ep.target_host.clone().into(),
        server_public_key,
        None,
    )
    .map_err(|e| format!("connect finalize (auth): {}", err_chain(&e)))?;

    // Now enter interactive mode: a short read timeout lets the active-stage
    // loop break to WouldBlock and service input while the server is idle.
    if let Some(s) = sock_handle {
        // Short timeout so the loop breaks to WouldBlock quickly to service
        // input — lower = snappier input, at the cost of more idle wakeups.
        let _ = s.set_read_timeout(Some(Duration::from_millis(12)));
    }

    Ok((result, upgraded_framed, shared, mic))
}

/// Walk an error's `source()` chain into one string — the real CredSSP/NTLM
/// cause is usually a few levels down, not in the top-level Display.
fn err_chain(e: &dyn std::error::Error) -> String {
    let mut out = e.to_string();
    let mut src = e.source();
    while let Some(s) = src {
        out.push_str(" -> ");
        out.push_str(&s.to_string());
        src = s.source();
    }
    out
}

/// Send ONLY the changed rectangle (tight-cropped RGBA), not the whole frame.
fn emit_rect(out: &Sink, image: &DecodedImage, rect: &InclusiveRectangle) {
    let bpp = image.bytes_per_pixel();
    let stride = image.stride();
    let x = rect.left as usize;
    let y = rect.top as usize;
    let w = (rect.right as usize).saturating_sub(x) + 1; // inclusive
    let h = (rect.bottom as usize).saturating_sub(y) + 1;
    if w == 0 || h == 0 {
        return;
    }
    let src = image.data();
    // Copy the rect row-by-row out of the full-width strided buffer.
    let mut tight = Vec::with_capacity(w * h * bpp);
    for row in 0..h {
        let start = (y + row) * stride + x * bpp;
        let end = start + w * bpp;
        if end <= src.len() {
            tight.extend_from_slice(&src[start..end]);
        }
    }
    // A short rect would not match its own header; Hostbay would refuse it.
    if tight.len() != w * h * 4 {
        return;
    }
    out.frame(x as u16, y as u16, w as u16, h as u16, &tight);
}

/// Connect and run the session until it ends. `tx` lets the clipboard poll
/// feed the same queue Hostbay's messages arrive on.
#[allow(clippy::too_many_arguments)]
pub fn run(
    out: Sink,
    rx: Receiver<In>,
    tx: Sender<In>,
    ep: Endpoint,
    user: String,
    password: String,
    width: u16,
    height: u16,
    security: String,
) -> AppResult<()> {
    // Split a domain out of the username if present (DOMAIN\user or user@domain)
    // — CredSSP/NTLM needs domain and user separated.
    let (user, domain) = if let Some((d, u)) = user.split_once('\\') {
        (u.to_string(), Some(d.to_string()))
    } else if let Some((u, d)) = user.split_once('@') {
        (u.to_string(), Some(d.to_string()))
    } else {
        (user, None)
    };

    // RDP servers sometimes reset the very first TCP/negotiation attempt
    // ("connection reset by peer"); retry a couple of times before giving up.
    let mut attempt = 0;
    let (conn_result, framed, clip, mic) = loop {
        attempt += 1;
        let cfg = build_config(user.clone(), password.clone(), domain.clone(), width, height, &security);
        match connect(&out, &rx, &ep, cfg) {
            Ok(ok) => break ok,
            Err(e) => {
                let msg = e.to_lowercase();
                let transient = msg.contains("reset by peer")
                    || msg.contains("os error 54")
                    || msg.contains("broken pipe")
                    || msg.contains("connection reset");
                if transient && attempt < 3 {
                    std::thread::sleep(Duration::from_millis(400));
                    continue;
                }
                return Err(e);
            }
        }
    };

    // Watch the native clipboard so a local copy propagates to the remote.
    spawn_file_clip_poll(tx, Arc::clone(&clip));

    let mut image = DecodedImage::new(
        PixelFormat::RgbA32,
        conn_result.desktop_size.width,
        conn_result.desktop_size.height,
    );
    run_active(&out, conn_result, framed, &mut image, rx, clip, mic)
}

/// Poll the native clipboard (macOS, Windows); when its contents change (and
/// it wasn't us who wrote them), advertise the new text or file selection to
/// the remote. Linux never changes here: Hostbay owns its clipboard (KTD3).
fn spawn_file_clip_poll(tx: Sender<In>, clip: Arc<Mutex<ClipShared>>) {
    std::thread::spawn(move || {
        let mut last = crate::pasteboard::change_count();
        loop {
            std::thread::sleep(Duration::from_millis(500));
            let now = crate::pasteboard::change_count();
            if now == last {
                continue;
            }
            last = now;
            // Skip the change we made ourselves (remote->local copy/download).
            if clip.lock().unwrap().self_written_change == now {
                continue;
            }
            // Files take priority; else text.
            let files = crate::pasteboard::read_files();
            let msg = if !files.is_empty() {
                In::ClipboardFiles { paths: files }
            } else {
                match crate::pasteboard::read_text() {
                    Some(text) if !text.is_empty() => In::ClipboardText { text },
                    _ => continue,
                }
            };
            if tx.send(msg).is_err() {
                return;
            }
        }
    });
}

/// Input operations for one `input` message; None for an unknown kind.
fn input_ops(kind: &str, x: u16, y: u16, button: u8, down: bool, scancode: u16, delta: i16) -> Option<Vec<Operation>> {
    let at = Operation::MouseMove(MousePosition { x, y });
    Some(match kind {
        "move" => vec![at],
        "button" => {
            // Browser PointerEvent.button: 0=left, 1=middle, 2=right.
            let btn = match button {
                0 => MouseButton::Left,
                2 => MouseButton::Right,
                _ => MouseButton::Middle,
            };
            vec![at, if down { Operation::MouseButtonPressed(btn) } else { Operation::MouseButtonReleased(btn) }]
        }
        "wheel" => vec![at, Operation::WheelRotations(WheelRotations { is_vertical: true, rotation_units: delta })],
        "key" => {
            let sc = Scancode::from_u16(scancode);
            vec![if down { Operation::KeyPressed(sc) } else { Operation::KeyReleased(sc) }]
        }
        _ => return None,
    })
}

fn run_active(
    out: &Sink,
    conn_result: connector::ConnectionResult,
    mut framed: UpgradedFramed,
    image: &mut DecodedImage,
    rx: Receiver<In>,
    clip: Arc<Mutex<ClipShared>>,
    mic: crate::audin::Mic,
) -> AppResult<()> {
    let lost = |e: std::io::Error| format!("connection lost: {e}");
    out.send(&Out::Size { width: conn_result.desktop_size.width, height: conn_result.desktop_size.height });
    let mut stage = ActiveStageBuilder {
        static_channels: conn_result.static_channels,
        user_channel_id: conn_result.user_channel_id,
        io_channel_id: conn_result.io_channel_id,
        message_channel_id: conn_result.message_channel_id,
        share_id: conn_result.share_id,
        compression_type: conn_result.compression_type,
        enable_server_pointer: conn_result.enable_server_pointer,
        pointer_software_rendering: conn_result.pointer_software_rendering,
    }
    .build();
    let mut input_db = Database::new();

    loop {
        // Drain input commands first.
        loop {
            let cmd = match rx.try_recv() {
                Ok(c) => c,
                Err(std::sync::mpsc::TryRecvError::Empty) => break,
                Err(std::sync::mpsc::TryRecvError::Disconnected) => return Ok(()),
            };
            let ops: Vec<Operation> = match cmd {
                In::Close => return Ok(()),
                // Local clipboard changed: stage the text and advertise a
                // copy so the remote can paste it. A fresh text copy replaces
                // any staged file selection.
                In::ClipboardText { text } => {
                    let mut s = clip.lock().unwrap();
                    s.local_text_utf16le = Some(text_to_utf16le(&text));
                    s.local_files.clear();
                    s.remote_file_format = None;
                    s.outbox.push(ClipIntent::CopyText);
                    continue;
                }
                // Local file selection changed: stage the flattened list
                // and advertise a file copy. Replaces any staged text.
                In::ClipboardFiles { paths } => {
                    let mut s = clip.lock().unwrap();
                    s.local_files = flatten_local(&paths);
                    eprintln!(
                        "[rdp-clip] local copy of {} path(s) -> {} descriptor(s)",
                        paths.len(),
                        s.local_files.len()
                    );
                    s.local_text_utf16le = None;
                    let descs: Vec<FileDescriptor> =
                        s.local_files.iter().map(|f| f.desc.clone()).collect();
                    s.outbox.push(ClipIntent::CopyFiles(descs));
                    continue;
                }
                In::Input { kind, x, y, button, down, scancode, delta } => {
                    match input_ops(&kind, x, y, button, down, scancode, delta) {
                        Some(ops) => ops,
                        None => continue,
                    }
                }
                // Connect and Trust only mean something before the session.
                In::Connect { .. } | In::Trust { .. } => continue,
            };
            let events = input_db.apply(ops);
            if !events.is_empty() {
                let outs = stage
                    .process_fastpath_input(image, &events)
                    .map_err(|e| format!("input: {e}"))?;
                for o in outs {
                    if let ActiveStageOutput::ResponseFrame(f) = o {
                        framed.write_all(&f).map_err(lost)?;
                    }
                }
            }
        }

        // Drain clipboard outbox: turn queued backend intents into channel PDUs.
        let intents: Vec<ClipIntent> = {
            let mut s = clip.lock().unwrap();
            if s.outbox.is_empty() { Vec::new() } else { std::mem::take(&mut s.outbox) }
        };
        for intent in intents {
            // File copy requires the channel to be Ready; if a file selection
            // arrived before then, re-stage it and let on_ready() flush it.
            if matches!(intent, ClipIntent::CopyFiles(_))
                && !clip.lock().unwrap().ready
            {
                continue;
            }
            let Some(cliprdr) = stage.get_svc_processor_mut::<CliprdrClient>() else { break };
            let label = intent.label();
            let result = match intent {
                ClipIntent::CopyText => cliprdr.initiate_copy(&[ClipboardFormat::new(
                    ClipboardFormatId::new(CF_UNICODETEXT as u32),
                )]),
                ClipIntent::CopyFiles(files) => cliprdr.initiate_file_copy(files),
                ClipIntent::Paste(format) => cliprdr.initiate_paste(format),
                ClipIntent::FormatData(resp) => cliprdr.submit_format_data(resp),
                ClipIntent::FileRequest(req) => cliprdr.request_file_contents(req),
                ClipIntent::FileResponse(resp) => cliprdr.submit_file_contents(resp),
            };
            match result {
                Ok(svc_msgs) => match stage.process_svc_processor_messages(svc_msgs) {
                    Ok(bytes) if !bytes.is_empty() => {
                        eprintln!("[rdp-clip] sent {label} ({} bytes)", bytes.len());
                        framed.write_all(&bytes).map_err(lost)?;
                    }
                    Ok(_) => eprintln!("[rdp-clip] {label} produced 0 bytes"),
                    Err(e) => eprintln!("[rdp-clip] encode {label} failed: {e}"),
                },
                Err(e) => eprintln!("[rdp-clip] {label} failed: {}", err_chain(&e)),
            }
        }

        // Drain captured mic PCM and push it over the AUDIO_INPUT DVC (when the
        // channel is open). get_dvc is Some only after the server opened it.
        let audin_channel_id = stage
            .get_dvc::<crate::audin::AudinChannel>()
            .map(|d| d.channel_id());
        if let Some(cid) = audin_channel_id {
            let pdus = crate::audin::take_pcm_pdus(&mic);
            if !pdus.is_empty() {
                match ironrdp::dvc::encode_dvc_messages(
                    cid,
                    pdus,
                    ironrdp::svc::ChannelFlags::empty(),
                ) {
                    Ok(svc_msgs) => match stage.encode_dvc_messages(svc_msgs) {
                        Ok(bytes) if !bytes.is_empty() => framed.write_all(&bytes).map_err(lost)?,
                        Ok(_) => {}
                        Err(e) => eprintln!("[rdp-audin] send failed: {e}"),
                    },
                    Err(e) => eprintln!("[rdp-audin] encode failed: {e}"),
                }
            }
        }

        match framed.read_pdu() {
            Ok((action, payload)) => {
                let outputs = stage
                    .process(image, action, &payload, framed.last_read_at())
                    .map_err(|e| format!("session: {e}"))?;
                for o in outputs {
                    match o {
                        ActiveStageOutput::ResponseFrame(frame) => framed.write_all(&frame).map_err(lost)?,
                        // Send ONLY the changed region — the big speedup.
                        ActiveStageOutput::GraphicsUpdate(rect) => emit_rect(out, image, &rect),
                        ActiveStageOutput::Terminate(_) => return Ok(()),
                        _ => {}
                    }
                }
            }
            // A read timeout is WouldBlock on Unix and TimedOut on Windows.
            Err(e) if matches!(e.kind(), std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut) => {
                // The 12ms socket read timeout already paces the loop; no extra
                // sleep needed (it only added input latency).
            }
            Err(e) => return Err(lost(e)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // A copied folder must flatten to: the dir descriptor (no relative_path) +
    // each child carrying the Windows-separated relative_path, and dl_target
    // must rebuild the same on-disk layout under a download dir.
    #[test]
    fn flatten_and_dl_target_roundtrip() {
        let tmp = std::env::temp_dir().join(format!("rdp-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&tmp);
        std::fs::create_dir_all(tmp.join("box/sub")).unwrap();
        std::fs::write(tmp.join("box/a.txt"), b"hi").unwrap();
        std::fs::write(tmp.join("box/sub/b.txt"), b"deep").unwrap();

        let files = flatten_local(&[tmp.join("box").to_string_lossy().into_owned()]);

        // Root dir descriptor: no relative_path, DIRECTORY attr.
        let root = files.iter().find(|f| f.desc.name == "box").unwrap();
        assert!(root.desc.relative_path.is_none());
        assert!(is_dir_desc(&root.desc));

        // Nested file carries the backslash path rooted at "box".
        let b = files.iter().find(|f| f.desc.name == "b.txt").unwrap();
        assert_eq!(b.desc.relative_path.as_deref(), Some("box\\sub"));
        assert_eq!(b.desc.file_size, Some(4));

        // dl_target rebuilds box/sub/b.txt under the download dir.
        let dir = tmp.join("dl");
        let target = dl_target(&dir, &b.desc);
        assert_eq!(target, dir.join("box").join("sub").join("b.txt"));

        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn read_slice_reads_range() {
        let tmp = std::env::temp_dir().join(format!("rdp-slice-{}", std::process::id()));
        std::fs::write(&tmp, b"0123456789").unwrap();
        assert_eq!(read_slice(&tmp, 2, 3).unwrap(), b"234");
        // Past EOF returns the tail, not padding.
        assert_eq!(read_slice(&tmp, 8, 100).unwrap(), b"89");
        let _ = std::fs::remove_file(&tmp);
    }

    // File names come from the server: none may climb out of the download dir.
    #[test]
    fn dl_target_stays_inside_the_download_dir() {
        let dir = std::path::Path::new("/tmp/dl");
        let d = FileDescriptor::new("..".to_owned()).with_relative_path("..\\..\\C:\\x".to_owned());
        assert_eq!(dl_target(dir, &d), dir.join("x").join("_"));
        let d = FileDescriptor::new("a/../../b".to_owned());
        assert_eq!(dl_target(dir, &d), dir.join("_"));
    }

    #[test]
    fn input_kinds_map_to_operations() {
        assert_eq!(input_ops("button", 1, 2, 2, true, 0, 0).unwrap().len(), 2);
        assert_eq!(input_ops("key", 0, 0, 0, false, 0xe04d, 0).unwrap().len(), 1);
        assert!(input_ops("teleport", 0, 0, 0, false, 0, 0).is_none());
    }

    // A MITM can replay the pinned (public) certificate; it must not be able
    // to sign the handshake with a different key. Fixtures: a self-signed
    // P-256 cert for key A, and ECDSA-SHA256 signatures by keys A and B.
    #[test]
    fn handshake_signature_must_match_the_presented_cert() {
        use base64::engine::general_purpose::STANDARD;
        use rustls::client::danger::ServerCertVerifier as _;
        // shortcut: rustls has no public DigitallySignedStruct constructor;
        // its doc-hidden Codec is the only way to build one in a test.
        use rustls::internal::msgs::codec::Codec as _;

        const CERT: &str = "MIIBhjCCASugAwIBAgIUEKGT/21WvvrWAR4uJP/xcRFn+GswCgYIKoZIzj0EAwIwFzEVMBMGA1UEAwwMaG9zdGJheS10ZXN0MCAXDTI2MTAyMzAyNTE0N1oYDzIxMjYwOTI5MDI1MTQ3WjAXMRUwEwYDVQQDDAxob3N0YmF5LXRlc3QwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAAS3WfWMRhc3xKuSQ/JjJY0VDvSKhjV7C/C7I6MHK2m3iH5tj8IAmZaZrloY0eKTzgn4ec3c2huDBgTMBIL2LB6To1MwUTAdBgNVHQ4EFgQUcohKEkH/2loVdxGAlf99YDhlLrwwHwYDVR0jBBgwFoAUcohKEkH/2loVdxGAlf99YDhlLrwwDwYDVR0TAQH/BAUwAwEB/zAKBggqhkjOPQQDAgNJADBGAiEAo1RCH+nzs7AZ1/SSQekQDXIMyjcTxefUkiTR/PAbH0ICIQDzsVmADgQfmPrADu9+mTxmh9TfglvcwWsUUxXHTryWCQ==";
        const SIG_A: &str = "MEQCIQCM4QwgZ1Y+rdrBvCHo/FGDu2KTUkCzsydHHJqmTxq+0QIfTF0xu51KQF4xnugWEKREApsVJVJT8Dv6K1kAL8fPGg==";
        const SIG_B: &str = "MEQCIDk2dRyMB6vsZSJDQ97K6fATt/dQKt7Q4+yu7E71gvpGAiB15P0keb25xDvW0VUV4twZgEdXpgIEvXokIzTno1ax7g==";
        let msg = b"hostbay handshake";
        let cert = rustls::pki_types::CertificateDer::from(STANDARD.decode(CERT).unwrap());
        let dss = |sig: &str| {
            let sig = STANDARD.decode(sig).unwrap();
            let mut wire = Vec::new();
            rustls::SignatureScheme::ECDSA_NISTP256_SHA256.encode(&mut wire);
            wire.extend_from_slice(&(sig.len() as u16).to_be_bytes());
            wire.extend_from_slice(&sig);
            rustls::DigitallySignedStruct::read_bytes(&wire).unwrap()
        };

        let v = danger::NoChainVerify::new();
        assert!(v.verify_tls12_signature(msg, &cert, &dss(SIG_A)).is_ok());
        assert!(v.verify_tls13_signature(msg, &cert, &dss(SIG_A)).is_ok());
        assert!(v.verify_tls12_signature(msg, &cert, &dss(SIG_B)).is_err());
        assert!(v.verify_tls13_signature(msg, &cert, &dss(SIG_B)).is_err());
    }

    #[test]
    fn nla_mode_never_offers_plain_tls() {
        let cfg = |s| build_config(String::new(), String::new(), None, 800, 600, s);
        let nla = cfg("nla");
        assert!(nla.enable_credssp && !nla.enable_tls);
        let tls = cfg("tls");
        assert!(tls.enable_tls && !tls.enable_credssp);
        let auto = cfg("auto");
        assert!(auto.enable_tls && auto.enable_credssp);
    }
}
