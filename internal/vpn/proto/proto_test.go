package proto

import (
	"encoding/json"
	"testing"
)

// The field names are the wire format both sides of an upgrade share, so a
// rename must show up here.
func TestWireNames(t *testing.T) {
	for _, c := range []struct {
		m    Msg
		want string
	}{
		{Msg{T: Hello, Version: Version}, `{"t":"hello","version":1}`},
		{Msg{T: Start, Type: TypeWireGuard, Config: "[Interface]", Secrets: map[string]string{"privateKey": "k"}, SocksUser: "u", SocksPass: "p"},
			`{"t":"start","type":"wireguard","config":"[Interface]","secrets":{"privateKey":"k"},"socksUser":"u","socksPass":"p"}`},
		{Msg{T: State, State: Connected, Address: []string{"10.0.0.1/32"}, Routes: []string{"10.0.0.0/24"}, SocksPort: 1080},
			`{"t":"state","state":"connected","address":["10.0.0.1/32"],"routes":["10.0.0.0/24"],"socksPort":1080}`},
		{Msg{T: State, State: Failed, Reason: "r"}, `{"t":"state","state":"failed","reason":"r"}`},
		{Msg{T: State, State: Failed, Reason: "r", Code: CodeCertChanged, ServerCert: "pin-sha256:x"},
			`{"t":"state","state":"failed","reason":"r","code":"cert-changed","serverCert":"pin-sha256:x"}`},
		{Msg{T: Need, Need: NeedBrowser, URL: "https://sso.example"}, `{"t":"need","need":"browser","url":"https://sso.example"}`},
		{Msg{T: Answer, OTP: "123456"}, `{"t":"answer","otp":"123456"}`},
		{Msg{T: NodeState, NodeState: "e30="}, `{"t":"nodeState","nodeState":"e30="}`},
	} {
		b, err := json.Marshal(c.m)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("got  %s\nwant %s", b, c.want)
		}
	}
}

// Dropping a type here silently stops its profiles from connecting, so the
// set is pinned.
func TestOpenConnectTypes(t *testing.T) {
	want := map[string]string{
		TypeAnyConnect:     "anyconnect",
		TypeSSLVPN:         "fortinet",
		TypeGlobalProtect:  "gp",
		TypePulse:          "pulse",
		TypeNetworkConnect: "nc",
		TypeF5:             "f5",
		TypeArray:          "array",
	}
	if len(OpenConnectTypes) != len(want) {
		t.Errorf("OpenConnectTypes has %d types, want %d", len(OpenConnectTypes), len(want))
	}
	for typ, p := range want {
		if OpenConnectTypes[typ] != p || !IsOpenConnect(typ) {
			t.Errorf("%s: protocol %q, IsOpenConnect %v; want %q, true", typ, OpenConnectTypes[typ], IsOpenConnect(typ), p)
		}
	}
	for _, typ := range []string{TypeWireGuard, TypeTailscale, TypeOpenVPN, TypeIPsec, ""} {
		if IsOpenConnect(typ) {
			t.Errorf("%q is not an OpenConnect type", typ)
		}
	}
}
