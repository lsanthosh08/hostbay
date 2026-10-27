//go:build !linux

package rdp

// On macOS and Windows the helper uses the native clipboard itself.

func (s *Session) watchClipboard() (stop func()) { return func() {} }

func (s *Session) remoteCopied(string) {}
