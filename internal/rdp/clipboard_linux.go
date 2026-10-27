package rdp

import "time"

// On Linux the helper has no window, and on GNOME Wayland only a focused
// window may own the clipboard, so Hostbay's own window serves it (KTD3):
// local changes are polled and sent to the helper, and text the remote copied
// is put on the window clipboard here. File copy is not served on Linux.

// clipPoll is how often the window clipboard is checked for a local copy.
const clipPoll = 500 * time.Millisecond

func (s *Session) watchClipboard() (stop func()) {
	cb := s.hooks.Clipboard
	if cb == nil {
		return func() {}
	}
	if text, ok := cb.Text(); ok {
		s.lastClip.Store(text)
	}
	quit := make(chan struct{})
	go func() {
		t := time.NewTicker(clipPoll)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
			}
			text, ok := cb.Text()
			// Skip what is already there, including what the remote just sent.
			if !ok || text == "" || s.lastClip.Load() == text {
				continue
			}
			s.lastClip.Store(text)
			_ = s.SetClipboardText(text) // invalid or oversized text stays local
		}
	}()
	return func() { close(quit) }
}

func (s *Session) remoteCopied(text string) {
	if cb := s.hooks.Clipboard; cb != nil {
		s.lastClip.Store(text)
		cb.SetText(text)
	}
}
