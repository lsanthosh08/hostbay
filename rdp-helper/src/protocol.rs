//! The control channel between Hostbay (Go) and this helper (KTD3).
//!
//! Both directions are newline-delimited JSON, one message per line, tagged
//! by `"t"`. The helper speaks first with `hello`; Go refuses a helper whose
//! version differs. The password arrives only inside `connect` on stdin.
//!
//! One exception on stdout: a `frame` line is followed by exactly `len` raw
//! RGBA bytes, so pixels never pay for base64 or JSON.
//!
//! Keep in step with `internal/rdp/protocol.go`.

use serde::{Deserialize, Serialize};
use std::io::{self, Write};
use std::sync::{Arc, Mutex};

/// Bumped on any incompatible change to the messages below.
pub const VERSION: u32 = 1;

/// Messages from Go, read from stdin.
#[derive(Deserialize)]
#[serde(tag = "t", rename_all = "camelCase", rename_all_fields = "camelCase")]
pub enum In {
    /// Start the session. `dial_*` is where to open TCP (a local forward when
    /// the host sits behind a jump chain); `target_*` is the real server,
    /// used for TLS SNI and CredSSP.
    Connect {
        dial_host: String,
        dial_port: u16,
        target_host: String,
        user: String,
        password: String,
        width: u16,
        height: u16,
        /// "auto" (TLS + CredSSP), "nla" or "tls" (no CredSSP, for xrdp).
        security: String,
    },
    /// Go's answer to `cert`: continue to credentials, or stop.
    Trust { accept: bool },
    /// Pointer and keyboard input, already range-checked by Go.
    Input {
        kind: String,
        x: u16,
        y: u16,
        button: u8,
        down: bool,
        scancode: u16,
        delta: i16,
    },
    /// Local clipboard text to offer to the remote.
    ClipboardText { text: String },
    /// Local files (absolute paths) to offer to the remote.
    ClipboardFiles { paths: Vec<String> },
    Close,
}

/// Messages to Go, written to stdout.
#[derive(Serialize, Debug, PartialEq)]
#[serde(tag = "t", rename_all = "camelCase", rename_all_fields = "camelCase")]
pub enum Out {
    Hello { version: u32 },
    /// The server's TLS leaf certificate (DER, base64). The helper sends no
    /// credential until Go answers with `trust`.
    Cert { der: String },
    /// Connected; also sent when the server changes the desktop size.
    Size { width: u16, height: u16 },
    /// Header of a changed rectangle; `len` RGBA bytes follow the newline.
    Frame { x: u16, y: u16, w: u16, h: u16, len: usize },
    /// The remote copied text (already on the native clipboard on macOS and
    /// Windows; Go puts it on the Linux clipboard).
    ClipboardText { text: String },
    /// Files the remote copied, downloaded to these local paths.
    ClipboardFiles { paths: Vec<String> },
    /// The session failed to start or ended with an error.
    Error { message: String },
}

pub fn parse(line: &str) -> Result<In, serde_json::Error> {
    serde_json::from_str(line)
}

/// Writes one message as a single line.
pub fn write_msg(w: &mut impl Write, msg: &Out) -> io::Result<()> {
    serde_json::to_writer(&mut *w, msg)?;
    w.write_all(b"\n")?;
    w.flush()
}

/// Writes a frame header line and its pixels as one unit.
pub fn write_frame(w: &mut impl Write, x: u16, y: u16, width: u16, height: u16, rgba: &[u8]) -> io::Result<()> {
    serde_json::to_writer(&mut *w, &Out::Frame { x, y, w: width, h: height, len: rgba.len() })?;
    w.write_all(b"\n")?;
    w.write_all(rgba)?;
    w.flush()
}

/// Shared stdout. Every message is written whole under the lock, so frames
/// from the session thread never interleave with other messages. A failed
/// write is ignored: Hostbay is gone, and stdin closing ends the process.
#[derive(Clone)]
pub struct Sink(Arc<Mutex<io::BufWriter<io::Stdout>>>);

impl Sink {
    pub fn stdout() -> Self {
        Sink(Arc::new(Mutex::new(io::BufWriter::new(io::stdout()))))
    }
    pub fn send(&self, msg: &Out) {
        let _ = write_msg(&mut *self.0.lock().unwrap(), msg);
    }
    pub fn frame(&self, x: u16, y: u16, w: u16, h: u16, rgba: &[u8]) {
        let _ = write_frame(&mut *self.0.lock().unwrap(), x, y, w, h, rgba);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hello_carries_the_version() {
        let mut b = Vec::new();
        write_msg(&mut b, &Out::Hello { version: VERSION }).unwrap();
        assert_eq!(b, b"{\"t\":\"hello\",\"version\":1}\n");
    }

    #[test]
    fn connect_parses_with_password_and_both_endpoints() {
        let m = parse(r#"{"t":"connect","dialHost":"127.0.0.1","dialPort":50123,"targetHost":"win.lan","user":"CORP\\me","password":"p\"w","width":1280,"height":800,"security":"auto"}"#).unwrap();
        match m {
            In::Connect { dial_host, dial_port, target_host, user, password, width, height, security } => {
                assert_eq!((dial_host.as_str(), dial_port), ("127.0.0.1", 50123));
                assert_eq!(target_host, "win.lan");
                assert_eq!(user, "CORP\\me");
                assert_eq!(password, "p\"w");
                assert_eq!((width, height), (1280, 800));
                assert_eq!(security, "auto");
            }
            _ => panic!("not connect"),
        }
    }

    #[test]
    fn input_and_control_messages_parse() {
        assert!(matches!(parse(r#"{"t":"trust","accept":true}"#).unwrap(), In::Trust { accept: true }));
        assert!(matches!(
            parse(r#"{"t":"input","kind":"key","x":0,"y":0,"button":0,"down":true,"scancode":57421,"delta":0}"#).unwrap(),
            In::Input { scancode: 0xe04d, down: true, .. }
        ));
        assert!(matches!(parse(r#"{"t":"clipboardText","text":"hi"}"#).unwrap(), In::ClipboardText { .. }));
        assert!(matches!(parse(r#"{"t":"clipboardFiles","paths":["/a"]}"#).unwrap(), In::ClipboardFiles { .. }));
        assert!(matches!(parse(r#"{"t":"close"}"#).unwrap(), In::Close));
    }

    #[test]
    fn malformed_and_unknown_messages_are_refused() {
        assert!(parse(r#"{"t":"launchMissiles"}"#).is_err());
        assert!(parse(r#"{"t":"input","kind":"move","x":-1,"y":0,"button":0,"down":false,"scancode":0,"delta":0}"#).is_err());
        assert!(parse(r#"{"t":"trust"}"#).is_err());
        assert!(parse("not json").is_err());
    }

    #[test]
    fn frame_is_a_header_line_then_raw_pixels() {
        let mut b = Vec::new();
        write_frame(&mut b, 1, 2, 1, 1, &[9, 8, 7, 6]).unwrap();
        let nl = b.iter().position(|&c| c == b'\n').unwrap();
        assert_eq!(&b[..nl], br#"{"t":"frame","x":1,"y":2,"w":1,"h":1,"len":4}"#);
        assert_eq!(&b[nl + 1..], &[9, 8, 7, 6]);
    }

    #[test]
    fn outgoing_messages_use_camel_case_tags() {
        let mut b = Vec::new();
        write_msg(&mut b, &Out::ClipboardFiles { paths: vec!["/tmp/x".into()] }).unwrap();
        assert_eq!(b, b"{\"t\":\"clipboardFiles\",\"paths\":[\"/tmp/x\"]}\n");
        b.clear();
        write_msg(&mut b, &Out::Cert { der: "AAE=".into() }).unwrap();
        assert_eq!(b, b"{\"t\":\"cert\",\"der\":\"AAE=\"}\n");
    }
}
