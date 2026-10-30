package vpn

import (
	"context"
	"errors"
	"time"

	"hostbay/internal/vpn/proto"
)

// BrowserPort asks the helper for its no-login SOCKS5 listener on
// 127.0.0.1, which it opens on the first request and keeps until it exits
// (KTD11). The port must not reach the UI, an event, a log or MCP.
func (h *Helper) BrowserPort(ctx context.Context) (int, error) {
	h.browserMu.Lock()
	defer h.browserMu.Unlock()
	select {
	case <-h.browser: // a reply whose caller gave up
	default:
	}
	if err := h.send(proto.Msg{T: proto.OpenBrowser}); err != nil {
		return 0, err
	}
	select {
	case m := <-h.browser:
		if m.SocksPort == 0 {
			return 0, errors.New(m.Reason)
		}
		return m.SocksPort, nil
	case <-h.done:
		return 0, errors.New("the VPN helper has stopped")
	case <-time.After(handshakeTimeout):
		return 0, errors.New("the VPN helper did not open the browser's SOCKS port")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
