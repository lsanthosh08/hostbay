//! Native Windows clipboard for RDP copy and paste: Unicode text and file
//! lists (CF_HDROP). Same functions as the macOS half of `pasteboard.rs`, so
//! the session code is shared.
//!
//! Errors read as an empty clipboard and failed writes are dropped: another
//! program holding the clipboard open must not end the RDP session.

use clipboard_win::{formats, options, raw, Clipboard};

/// GetClipboardSequenceNumber: bumps on every clipboard write by anyone.
pub fn change_count() -> isize {
    raw::seq_num().map(|n| n.get() as isize).unwrap_or(0)
}

pub fn read_text() -> Option<String> {
    clipboard_win::get_clipboard_string().ok()
}

pub fn write_text(text: &str) {
    let _ = clipboard_win::set_clipboard_string(text);
}

/// Absolute paths currently on the clipboard as a file list.
pub fn read_files() -> Vec<String> {
    clipboard_win::get_clipboard::<Vec<String>, _>(formats::FileList).unwrap_or_default()
}

/// Replace the clipboard with a file list (absolute paths).
pub fn write_files(paths: &[String]) {
    if let Ok(_open) = Clipboard::new_attempts(10) {
        let _ = raw::set_file_list_with(paths, options::DoClear);
    }
}
