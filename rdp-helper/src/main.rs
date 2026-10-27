//! Hostbay's RDP helper. Hostbay starts one per RDP session and drives it
//! over stdin/stdout (see `protocol.rs`); diagnostics go to stderr, never
//! the password. The helper exits when its stdin closes, so it cannot
//! outlive Hostbay.

mod audin;
mod audio;
#[cfg(windows)]
mod clipboard_windows;
mod pasteboard;
mod protocol;
mod rdp;

use protocol::{In, Out, Sink};
use std::io::BufRead;
use std::sync::mpsc;

fn main() {
    let out = Sink::stdout();
    out.send(&Out::Hello { version: protocol::VERSION });

    let (tx, rx) = mpsc::channel::<In>();
    let reader_tx = tx.clone();
    std::thread::spawn(move || {
        for line in std::io::stdin().lock().lines() {
            let Ok(line) = line else { break };
            if line.trim().is_empty() {
                continue;
            }
            match protocol::parse(&line) {
                // serde_json's error names the position, not the input, so
                // a malformed connect message cannot leak its password here.
                Err(e) => eprintln!("[rdp-helper] ignored a malformed message: {e}"),
                Ok(msg) => {
                    if reader_tx.send(msg).is_err() {
                        break;
                    }
                }
            }
        }
        // Hostbay closed our stdin: it quit, crashed or ended the session.
        std::process::exit(0);
    });

    let result = match rx.recv() {
        Ok(In::Connect { dial_host, dial_port, target_host, user, password, width, height, security }) => rdp::run(
            out.clone(),
            rx,
            tx,
            rdp::Endpoint { dial_host, dial_port, target_host },
            user,
            password,
            width,
            height,
            security,
        ),
        Ok(_) => Err("the first message must be connect".into()),
        Err(_) => Ok(()),
    };
    if let Err(message) = result {
        out.send(&Out::Error { message });
        std::process::exit(1);
    }
}
