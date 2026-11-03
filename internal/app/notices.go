package app

import (
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"hostbay/internal/agentexec"
)

// noticeMax is the longest request an OS notification carries with Allow
// and Deny on it. A longer one is cut on screen, and an Allow on text the
// user cannot read in full would approve something unseen, so it gets only
// a click that opens the window and its card.
const noticeMax = 180

// Notice action IDs.
const (
	noticeAllow = "allow"
	noticeDeny  = "deny"
)

const noticeCategory = "mcp-approval"

// noticeSender shows and withdraws OS notifications. Send calls undelivered,
// off the caller's goroutine, if the notification could not be shown.
type noticeSender interface {
	Send(id, title, body string, actions bool, undelivered func()) error
	Clear(id string)
}

// noticeText is what a waiting MCP request says in a notification, and
// whether all of it fits, so Allow and Deny can sit on it.
func noticeText(p agentexec.Pending) (title, body string, actions bool) {
	client := p.Client
	if client == "" {
		client = "An MCP client"
	}
	switch p.Tool {
	case "connect_vpn", "disconnect_vpn":
		verb := "connect"
		if p.Tool == "disconnect_vpn" {
			verb = "disconnect"
		}
		// Everything the card shows: a system-wide profile changes every
		// connection on this computer, which a name alone would hide.
		body = verb + " VPN " + p.Host
		if p.Tier != "" {
			body += ", tier " + p.Tier
		}
		if p.Gateway != "" {
			body += ", gateway " + p.Gateway
		}
		if p.Tag != "" {
			body += ", tag " + p.Tag
		}
	default:
		body = p.Host + ": " + p.Bytes
	}
	title = "Hostbay: " + client + " needs approval"
	actions = !strings.ContainsAny(body, "\r\n") && utf8.RuneCountInString(body) <= noticeMax
	if !actions {
		r := []rune(strings.NewReplacer("\r", " ", "\n", " ").Replace(body))
		body = string(r[:min(len(r), noticeMax)]) + "…"
	}
	return title, body, actions
}

// osNotices is noticeSender on the system's notification centre.
type osNotices struct {
	ns   *notifications.NotificationService
	once sync.Once
	ok   bool
	on   func(id, action string)

	io   sync.Mutex      // one call into the system at a time
	mu   sync.Mutex      // guards the maps, never held across a call into the system
	sent map[string]bool // delivered, not yet withdrawn
	todo map[string]bool // asked to Send, not yet run; true when withdrawn since
}

// LinkNotices shows each MCP request that waits as an OS notification with
// Allow and Deny, so a user in another app can answer without switching to
// Hostbay. Where a notification cannot be shown the window comes forward and
// the card is there as before.
func LinkNotices(m *MCPService, ns *notifications.NotificationService) {
	o := &osNotices{ns: ns, on: m.noticeAnswered, sent: map[string]bool{}, todo: map[string]bool{}}
	m.mu.Lock()
	m.notice = o
	m.mu.Unlock()
}

// setup asks for permission and registers the category, once, the first
// time a request waits: macOS asks the user then, with a reason in view.
func (o *osNotices) setup() bool {
	o.once.Do(func() {
		if ok, err := o.ns.RequestNotificationAuthorization(); err != nil || !ok {
			return
		}
		err := o.ns.RegisterNotificationCategory(notifications.NotificationCategory{
			ID: noticeCategory,
			Actions: []notifications.NotificationAction{
				{ID: noticeAllow, Title: "Allow once"},
				{ID: noticeDeny, Title: "Deny", Destructive: true},
			},
		})
		if err != nil {
			return
		}
		o.ns.OnNotificationResponse(func(r notifications.NotificationResult) {
			if r.Error != nil {
				return
			}
			act := r.Response.ActionIdentifier
			if act != noticeAllow && act != noticeDeny {
				act = ""
			}
			o.on(r.Response.ID, act)
		})
		o.ok = true
	})
	return o.ok
}

// Send posts the notification off the caller's goroutine: the first one
// asks macOS for permission and waits for the answer, and the request it is
// for is waiting on its caller.
func (o *osNotices) Send(id, title, body string, actions bool, undelivered func()) error {
	o.mu.Lock()
	o.todo[id] = false
	o.mu.Unlock()
	go func() {
		o.io.Lock()
		defer o.io.Unlock()
		o.mu.Lock()
		withdrawn := o.todo[id]
		delete(o.todo, id)
		o.mu.Unlock()
		if withdrawn {
			return
		}
		if !o.setup() {
			undelivered()
			return
		}
		opts := notifications.NotificationOptions{ID: id, Title: title, Body: body, InterruptionLevel: notifications.InterruptionLevelTimeSensitive}
		send := o.ns.SendNotification
		if actions {
			opts.CategoryID = noticeCategory
			send = o.ns.SendNotificationWithActions
		}
		if send(opts) != nil {
			undelivered()
			return
		}
		o.mu.Lock()
		o.sent[id] = true
		o.mu.Unlock()
	}()
	return nil
}

// Clear withdraws the notification, or stops a Send that has not run yet.
// It waits its turn behind a Send in progress, off the caller's goroutine.
func (o *osNotices) Clear(id string) {
	o.mu.Lock()
	if _, queued := o.todo[id]; queued {
		o.todo[id] = true
	}
	o.mu.Unlock()
	go func() {
		o.io.Lock()
		defer o.io.Unlock()
		o.mu.Lock()
		was := o.sent[id]
		delete(o.sent, id)
		o.mu.Unlock()
		if was {
			_ = o.ns.RemoveNotification(id)
		}
	}()
}
