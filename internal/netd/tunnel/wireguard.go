package tunnel

import (
	"context"
	"sync"

	"golang.zx2c4.com/wireguard/tun"

	"hostbay/internal/netd"
	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/wg"
)

// onceTUN closes its device once, however many owners close it.
type onceTUN struct {
	tun.Device
	once sync.Once
}

func (t *onceTUN) Close() error {
	var err error
	t.once.Do(func() { err = t.Device.Close() })
	return err
}

// wireGuard brings up a WireGuard tunnel on a real TUN configured from the
// profile, with the app-scoped engine's handshake watch: up after the first
// handshake, ended when none comes or they stop (internal/vpn/wg).
func (m *Manager) wireGuard(h string, w *netd.WireGuard, up func(string, []string), ended func(string)) (netd.Msg, *netd.Error) {
	var dev *onceTUN
	e, err := wg.NewSystem(w.Conf, map[string]string{"privateKey": w.PrivateKey, "presharedKey": w.PresharedKey}, func(mtu int) (tun.Device, error) {
		d, err := m.createTUN(tunName(h), mtu)
		if err == nil {
			dev = &onceTUN{Device: d}
		}
		return dev, err
	})
	if err != nil {
		return netd.Msg{}, refused("WireGuard: " + err.Error())
	}
	name, err := dev.Name()
	if err == nil {
		err = m.setAddrs(dev.Device, name, e.Addresses())
	}
	if err != nil {
		dev.Close()
		return netd.Msg{}, failed("setting up the WireGuard interface: " + err.Error())
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	end := m.add(h, func() { cancel(); <-ran })
	go func() {
		reason := "WireGuard stopped."
		e.Run(ctx, func(pm proto.Msg) {
			switch pm.State {
			case proto.Connected:
				up(name, pm.Address)
			case proto.Failed, proto.Dropped:
				reason = pm.Reason
			}
		})
		dev.Close()
		close(ran)
		end()
		ended(reason)
	}()
	return netd.Msg{Iface: name}, nil
}
