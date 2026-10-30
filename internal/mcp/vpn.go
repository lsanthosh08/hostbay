package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"hostbay/internal/agentexec"
	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

// VPN tools (R34, R35, KTD6). A connect or disconnect asks in the approval
// tray unless the user allowed that profile for that token in Settings; a
// system-wide profile always asks. An agent's dial never connects a VPN:
// it fails naming the profile, and the agent calls connect_vpn.
const (
	ToolConnectVPN    = "connect_vpn"
	ToolDisconnectVPN = "disconnect_vpn"
)

// connectWait bounds how long connect_vpn waits, once allowed, for the
// profile to come up; the connection carries on after it.
const connectWait = 20 * time.Second

// VPNs is the app's VPN service as these tools reach it. A connection
// remembers who made it, so a token disconnects only its own.
type VPNs interface {
	State(id string) VPNLive
	Connect(ctx context.Context, id, principal string) (VPNLive, error)
	// Disconnect stops the profile only if principal connected it.
	Disconnect(id, principal string) error
}

// VPNLive is a profile's live state. It never carries the SOCKS port.
type VPNLive struct {
	Status string // "" is disconnected
	Reason string
	Need   string
	By     string // who holds it up: "" no one, "user", or an MCP principal
}

// VPNView is a VPN profile as a client sees it: no secret, user or port.
type VPNView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Tier        string   `json:"tier"` // app, system (the whole computer) or external
	Gateway     string   `json:"gateway,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	State       string   `json:"state"`
	Reason      string   `json:"reason,omitempty" jsonschema:"why the profile is failed or dropped, when it is"`
	Need        string   `json:"need,omitempty" jsonschema:"what signing_in waits for the user to do in Hostbay or their browser: a one-time code or a browser sign-in"`
	ConnectedBy string   `json:"connectedBy,omitempty"` // you, user or another client
}

type vpnList struct {
	VPNs []VPNView `json:"vpns"`
}

type vpnFilter struct {
	Tag  string `json:"tag,omitempty" jsonschema:"only profiles with this tag, matched without regard to case"`
	Text string `json:"text,omitempty" jsonschema:"only profiles whose name holds this text, without regard to case"`
}

type vpnRef struct {
	ProfileID string `json:"profileId" jsonschema:"a VPN profile's id, from list_vpns or list_connections"`
}

type vpnConnect struct {
	ProfileID string `json:"profileId,omitempty" jsonschema:"the VPN profile's id; give this or tag"`
	Tag       string `json:"tag,omitempty" jsonschema:"connect the one profile with this tag; when several have it, nothing connects and the error names them"`
}

func (s *Server) addVPNTools(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "list_vpns", Description: "Lists the user's VPN profiles with their state and who connected each, filtered by tag or name."}, s.listVPNs)
	sdk.AddTool(srv, &sdk.Tool{Name: "vpn_status", Description: "Reports one VPN profile's state."}, s.vpnStatus)
	sdk.AddTool(srv, &sdk.Tool{Name: ToolConnectVPN, Description: "Connects a VPN profile, by id or by a tag only one profile has. It waits for the user to approve it in Hostbay, then up to 20 seconds for the profile to come up; call vpn_status while it is still connecting. A profile that connects in its own app (tier external) is not connected here: ask the user to open it. A state of signing_in (a one-time code or a browser sign-in) needs the user, so tell them and call vpn_status until it connects."}, s.connectVPN)
	sdk.AddTool(srv, &sdk.Tool{Name: ToolDisconnectVPN, Description: "Disconnects a VPN profile you connected, after the user approves it in Hostbay. Profiles the user or another client connected are refused."}, s.disconnectVPN)
}

func (s *Server) vpnView(v *store.VPN, principal string) VPNView {
	l := s.VPNs.State(v.ID)
	out := VPNView{ID: v.ID, Name: v.Name, Type: v.Type, Tier: v.Tier, Gateway: v.Gateway, Tags: v.Tags,
		State: cmp.Or(l.Status, "disconnected"), Reason: l.Reason, Need: l.Need}
	switch l.By {
	case "":
	case principal:
		out.ConnectedBy = "you"
	case "user":
		out.ConnectedBy = "user"
	default:
		out.ConnectedBy = "another client"
	}
	return out
}

func vpnByID(d *store.Data, id string) (*store.VPN, error) {
	if v := d.VPN(id); v != nil {
		return v, nil
	}
	return nil, errors.New("no VPN profile has that id; call list_vpns")
}

func hasTag(tags []string, tag string) bool {
	return slices.ContainsFunc(tags, func(t string) bool { return strings.EqualFold(t, tag) })
}

func (s *Server) listVPNs(_ context.Context, req *sdk.CallToolRequest, in vpnFilter) (*sdk.CallToolResult, vpnList, error) {
	p, _ := caller(req.Extra.TokenInfo)
	d, err := s.St.Data()
	if err != nil {
		return nil, vpnList{}, err
	}
	out := vpnList{VPNs: []VPNView{}}
	for i := range d.VPNs.Items {
		v := &d.VPNs.Items[i]
		if in.Tag != "" && !hasTag(v.Tags, in.Tag) || !strings.Contains(strings.ToLower(v.Name), strings.ToLower(in.Text)) {
			continue
		}
		out.VPNs = append(out.VPNs, s.vpnView(v, p))
	}
	return nil, out, nil
}

func (s *Server) vpnStatus(_ context.Context, req *sdk.CallToolRequest, in vpnRef) (*sdk.CallToolResult, VPNView, error) {
	p, _ := caller(req.Extra.TokenInfo)
	d, err := s.St.Data()
	if err != nil {
		return nil, VPNView{}, err
	}
	v, err := vpnByID(d, in.ProfileID)
	if err != nil {
		return nil, VPNView{}, err
	}
	return nil, s.vpnView(v, p), nil
}

// pickVPN is the profile id names, or the one profile with tag (R35).
// Several with the tag is an error naming them all, and nothing connects.
func pickVPN(d *store.Data, id, tag string) (*store.VPN, error) {
	switch {
	case (id == "") == (tag == ""):
		return nil, errors.New("give a profileId or a tag, not both")
	case id != "":
		return vpnByID(d, id)
	}
	var hits []*store.VPN
	for i := range d.VPNs.Items {
		if hasTag(d.VPNs.Items[i].Tags, tag) {
			hits = append(hits, &d.VPNs.Items[i])
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no VPN profile has the tag %q; call list_vpns", tag)
	case 1:
		return hits[0], nil
	}
	var names, ids []string
	for _, v := range hits {
		names, ids = append(names, fmt.Sprintf("%s (%s)", v.Name, v.ID)), append(ids, v.ID)
	}
	return nil, apperr.New(apperr.VPNAmbiguous, fmt.Sprintf("%d VPN profiles have the tag %q: %s. Nothing connected; call connect_vpn with one profileId.",
		len(hits), tag, strings.Join(names, ", "))).With("vpnIds", strings.Join(ids, ","))
}

func (s *Server) connectVPN(ctx context.Context, req *sdk.CallToolRequest, in vpnConnect) (*sdk.CallToolResult, VPNView, error) {
	p, client := caller(req.Extra.TokenInfo)
	d, err := s.St.Data()
	if err != nil {
		return nil, VPNView{}, err
	}
	v, err := pickVPN(d, in.ProfileID, in.Tag)
	if err != nil {
		return nil, VPNView{}, clientErr(err)
	}
	// Held up already, or an external tunnel the user's own app brought up
	// (it has no run, so no By): nothing changes, so no card.
	if l := s.VPNs.State(v.ID); l.By != "" || l.Status == "connected" {
		return nil, s.vpnView(v, p), nil
	}
	// Hostbay never connects an external profile, so asking would only
	// end in a refusal.
	if v.Tier == store.VPNTierExternal {
		return nil, VPNView{}, clientErr(apperr.New(apperr.VPNFailed, "VPN "+v.Name+" connects in its own app, not through Hostbay. Ask the user to open it there, then call vpn_status."))
	}
	if err := s.gateVPN(ctx, p, client, ToolConnectVPN, v, in.Tag); err != nil {
		return nil, VPNView{}, clientErr(err)
	}
	wait, cancel := context.WithTimeout(clientGone(ctx), connectWait)
	defer cancel()
	if _, err := s.VPNs.Connect(wait, v.ID, p); err != nil && wait.Err() == nil {
		return nil, VPNView{}, clientErr(err)
	}
	return nil, s.vpnView(v, p), nil
}

func (s *Server) disconnectVPN(ctx context.Context, req *sdk.CallToolRequest, in vpnRef) (*sdk.CallToolResult, VPNView, error) {
	p, client := caller(req.Extra.TokenInfo)
	d, err := s.St.Data()
	if err != nil {
		return nil, VPNView{}, err
	}
	v, err := vpnByID(d, in.ProfileID)
	if err != nil {
		return nil, VPNView{}, err
	}
	switch by := s.VPNs.State(v.ID).By; {
	case by == "":
		return nil, VPNView{}, fmt.Errorf("VPN %s is not connected", v.Name)
	case by != p:
		return nil, VPNView{}, fmt.Errorf("VPN %s was connected by the user or another client, so only they can disconnect it", v.Name)
	}
	if err := s.gateVPN(ctx, p, client, ToolDisconnectVPN, v, ""); err != nil {
		return nil, VPNView{}, clientErr(err)
	}
	if err := s.VPNs.Disconnect(v.ID, p); err != nil {
		return nil, VPNView{}, clientErr(err)
	}
	return nil, s.vpnView(v, p), nil
}

// unseen is a character an approval card cannot show as it is.
func unseen(r rune) bool { return unicode.IsControl(r) || store.Invisible(r) }

// gateVPN lets principal connect or disconnect v: at once with an allow
// entry for this token and the profile as it is now, otherwise once the
// user approves a card naming the client, profile, tag, tier and gateway.
// A profile whose type, gateway or tier changed while the card waited is
// refused, since the card showed the old one.
func (s *Server) gateVPN(ctx context.Context, principal, client, tool string, v *store.VPN, tag string) error {
	for _, f := range [][2]string{{"profile name", v.Name}, {"gateway", v.Gateway}, {"tag", tag}} {
		if i := strings.IndexFunc(f[1], unseen); i >= 0 {
			r, _ := utf8.DecodeRuneInString(f[1][i:])
			return fmt.Errorf("the VPN %s holds the control or invisible character U+%04X, which an approval card cannot show; nothing changed", f[0], r)
		}
	}
	l, err := s.St.Local()
	if err != nil {
		return err
	}
	token := strings.TrimPrefix(principal, principalScope)
	if slices.ContainsFunc(l.MCP.VPNAllow, func(e store.VPNAllowEntry) bool { return e.Matches(token, v) }) {
		return nil
	}
	_, err = s.Jobs.Approvals.Ask(clientGone(ctx), agentexec.Pending{Principal: principal, Client: client, Target: v.ID, Host: v.Name,
		Tool: tool, Tag: tag, Tier: v.Tier, Gateway: v.Gateway})
	switch c := apperr.CodeOf(err); {
	case c == apperr.ApprovalDenied:
		return apperr.New(c, "The user denied this request; nothing changed on VPN "+v.Name+".")
	case c == apperr.ApprovalExpired:
		return apperr.New(c, "No one answered within 110 seconds, so the request was denied; nothing changed on VPN "+v.Name+".")
	case err != nil:
		return err
	}
	d, err := s.St.Data()
	if err != nil {
		return err
	}
	if now := d.VPN(v.ID); now == nil || now.Type != v.Type || now.Gateway != v.Gateway || now.Tier != v.Tier {
		return fmt.Errorf("VPN %s changed while the request waited for approval; nothing changed, so ask again", v.Name)
	}
	return nil
}
