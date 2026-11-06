package app

import (
	"context"
	"net"
	"slices"
	"strconv"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/vpn/vpntest"
)

// vpnTunnels is a tunnel service whose VPN profiles start the fake helper in
// mode, with one app-scoped profile saved.
func vpnTunnels(t *testing.T, mode string) (*TunnelService, *ConnService, VPN) {
	t.Helper()
	c, s, _ := vpnConn(t, mode)
	p := saveProfile(t, c.creds.vpns, "office", nil)
	svc := NewTunnelService(s.st, c)
	svc.emit = func(string, any) {}
	t.Cleanup(func() { svc.ServiceShutdown() })
	return svc, c, p
}

// A forward through a connected app-scoped profile carries a client outside
// Hostbay to the destination, which the profile is asked for by name.
func TestVPNForwardReachesTheDestinationThroughTheProfile(t *testing.T) {
	svc, c, p := vpnTunnels(t, vpntest.Down)
	host, port := echoAddr(t)
	e := newVPNEndpoint(t, net.JoinHostPort(host, strconv.Itoa(port)))
	up(c.creds.vpns, p.ID, e)
	tn, err := svc.SaveTunnel(Tunnel{Name: "db", Kind: "local", VPNID: p.ID, TargetHost: "db.internal", TargetPort: 5432})
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.StartTunnel(context.Background(), tn.ID)
	if err != nil || st.Status != TunnelListening {
		t.Fatalf("start = %+v, %v", st, err)
	}
	echoesThrough(t, st.Addr)
	if got := e.asked(); !slices.Equal(got, []string{"db.internal:5432"}) {
		t.Fatalf("profile asked for %v, want [db.internal:5432]", got)
	}
	if err := svc.StopTunnel(tn.ID); err != nil {
		t.Fatal(err)
	}
}

// A forward whose profile cannot connect fails to start naming the profile;
// it never listens, so nothing can go direct.
func TestVPNForwardWithProfileDownFailsToStart(t *testing.T) {
	svc, _, p := vpnTunnels(t, vpntest.Down)
	tn, err := svc.SaveTunnel(Tunnel{Name: "db", Kind: "local", VPNID: p.ID, TargetHost: "db.internal", TargetPort: 5432})
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.StartTunnel(context.Background(), tn.ID)
	vpnCode(t, err, apperr.VPNFailed, p.ID)
	if st.Status != TunnelFailed || svc.RunningTunnels() != 0 {
		t.Fatalf("state = %+v, running %d", st, svc.RunningTunnels())
	}
}

// A forward whose profile was deleted says so instead of dialling direct.
func TestVPNForwardWithDeletedProfileFails(t *testing.T) {
	svc, c, p := vpnTunnels(t, vpntest.Down)
	tn, err := svc.SaveTunnel(Tunnel{Name: "db", Kind: "local", VPNID: p.ID, TargetHost: "db.internal", TargetPort: 5432})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.creds.vpns.DeleteVPN(p.ID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.StartTunnel(context.Background(), tn.ID)
	ae := vpnCode(t, err, apperr.VPNFailed, p.ID)
	if ae.Details["reason"] != "missing" {
		t.Fatalf("details = %v, want reason missing", ae.Details)
	}
}

func TestSaveTunnelNeedsOneHostOrOneLocalVPN(t *testing.T) {
	svc, _, p := vpnTunnels(t, vpntest.Down)
	e := newTunnelEnv(t)
	for _, c := range []struct {
		name string
		svc  *TunnelService
		tn   Tunnel
	}{
		{"neither", svc, Tunnel{Kind: "local", TargetHost: "db", TargetPort: 1}},
		{"both", e.svc, Tunnel{Kind: "local", HostID: e.hostID, VPNID: p.ID, TargetHost: "db", TargetPort: 1}},
		{"remote via VPN", svc, Tunnel{Kind: "remote", VPNID: p.ID, BindPort: 9000, TargetHost: "127.0.0.1", TargetPort: 1}},
		{"dynamic via VPN", svc, Tunnel{Kind: "dynamic", VPNID: p.ID}},
		{"unknown VPN", svc, Tunnel{Kind: "local", VPNID: "nope", TargetHost: "db", TargetPort: 1}},
	} {
		if _, err := c.svc.SaveTunnel(c.tn); err == nil {
			t.Errorf("%s: saved", c.name)
		}
	}
}
