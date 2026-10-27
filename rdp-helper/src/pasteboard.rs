//! Native clipboard access for RDP copy and paste: NSPasteboard on macOS,
//! `clipboard_windows.rs` on Windows. Linux has no helper-side clipboard:
//! the Hostbay window owns it there (KTD3) and text crosses the control
//! channel instead.
//!
//! macOS: native clipboard file access (NSPasteboard).
//!
//! The webview `navigator.clipboard` can only hold text/images, so file
//! copy/paste over RDP needs the real OS pasteboard. We read/write the file
//! list via the (deprecated but universally-honored) `NSFilenamesPboardType`
//! property list — an array of absolute path strings.
//!
//! ponytail: NSFilenamesPboardType path-array is the least objc2 marshaling
//! for both directions. Ceiling: it's deprecated — if a future macOS drops it,
//! switch to readObjectsForClasses:[NSURL] / NSPasteboardTypeFileURL items.

#[cfg(target_os = "macos")]
#[allow(deprecated)] // NSFilenamesPboardType — see module doc
mod imp {
    use objc2::runtime::AnyObject;
    use objc2_app_kit::{NSFilenamesPboardType, NSPasteboard, NSPasteboardTypeString};
    use objc2_foundation::{NSArray, NSString};

    /// Plain-text currently on the clipboard, if any.
    pub fn read_text() -> Option<String> {
        unsafe {
            NSPasteboard::generalPasteboard()
                .stringForType(NSPasteboardTypeString)
                .map(|s| s.to_string())
        }
    }

    /// Replace the clipboard with plain text.
    pub fn write_text(text: &str) {
        unsafe {
            let pb = NSPasteboard::generalPasteboard();
            pb.clearContents();
            pb.setString_forType(&NSString::from_str(text), NSPasteboardTypeString);
        }
    }

    /// Current pasteboard changeCount — bumps on every write by anyone. Used to
    /// poll for "did the file selection change" cheaply.
    pub fn change_count() -> isize {
        NSPasteboard::generalPasteboard().changeCount()
    }

    /// Absolute paths currently on the clipboard as a file list (empty if the
    /// clipboard holds no files).
    pub fn read_files() -> Vec<String> {
        unsafe {
            let pb = NSPasteboard::generalPasteboard();
            let Some(plist) = pb.propertyListForType(NSFilenamesPboardType) else {
                return Vec::new();
            };
            // The plist for NSFilenamesPboardType is an NSArray of NSStrings.
            // Downcast the untyped array, then each element to NSString.
            let Ok(arr) = plist.downcast::<NSArray>() else {
                return Vec::new();
            };
            arr.iter()
                .filter_map(|o: objc2::rc::Retained<AnyObject>| {
                    o.downcast::<NSString>().ok().map(|s| s.to_string())
                })
                .collect()
        }
    }

    /// Replace the clipboard with a file list (absolute paths).
    pub fn write_files(paths: &[String]) {
        unsafe {
            let pb = NSPasteboard::generalPasteboard();
            let ns: Vec<_> = paths.iter().map(|p| NSString::from_str(p)).collect();
            let arr = NSArray::from_retained_slice(&ns);
            let types = NSArray::from_slice(&[NSFilenamesPboardType]);
            pb.clearContents();
            pb.declareTypes_owner(&types, None);
            pb.setPropertyList_forType(&arr, NSFilenamesPboardType);
        }
    }
}

#[cfg(windows)]
use crate::clipboard_windows as imp;

#[cfg(not(any(target_os = "macos", windows)))]
mod imp {
    pub fn change_count() -> isize {
        0
    }
    pub fn read_files() -> Vec<String> {
        Vec::new()
    }
    pub fn write_files(_paths: &[String]) {}
    pub fn read_text() -> Option<String> {
        None
    }
    pub fn write_text(_text: &str) {}
}

pub use imp::{change_count, read_files, read_text, write_files, write_text};
