// Command hostbay-vpn runs one app-scoped VPN connection for Hostbay (KTD1).
// Hostbay starts one per connected profile, by absolute path and with no
// arguments, and drives it over stdin and stdout with the messages in
// internal/vpn/proto: the helper sends hello, reads one start message, brings
// the tunnel up inside its own process and serves an authenticated SOCKS5
// endpoint on 127.0.0.1 through it (KTD2), reporting each state change. It
// exits when its stdin closes, taking the tunnel and the listener with it.
//
// Secrets arrive only in the start message. Nothing the helper writes, to
// stdout or stderr, carries one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/socks"
	"hostbay/internal/vpn/wg"
)

// engine is one VPN type running in the helper.
type engine interface {
	// Run brings the tunnel up and reports state messages until ctx ends or
	// the tunnel fails or drops.
	Run(ctx context.Context, report func(proto.Msg))
	// Dial opens a "tcp" connection or a connected "udp" socket through the
	// tunnel, resolving names through the tunnel's DNS.
	Dial(ctx context.Context, network, address string) (net.Conn, error)
}

var engines = map[string]func(config string, secrets map[string]string) (engine, error){
	proto.TypeWireGuard: func(c string, s map[string]string) (engine, error) { return wg.New(c, s) },
}

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hostbay-vpn:", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer) error {
	var wmu sync.Mutex // one message per write
	enc := json.NewEncoder(out)
	send := func(m proto.Msg) {
		wmu.Lock()
		enc.Encode(m)
		wmu.Unlock()
	}
	fail := func(reason string) error {
		send(proto.Msg{T: proto.State, State: proto.Failed, Reason: reason})
		return errors.New(reason)
	}

	send(proto.Msg{T: proto.Hello, Version: proto.Version})
	dec := json.NewDecoder(in)
	var start proto.Msg
	if err := dec.Decode(&start); err != nil {
		return fmt.Errorf("reading the start message: %w", err)
	}
	if start.T != proto.Start {
		return fail(fmt.Sprintf("Expected a start message, got %q.", start.T))
	}
	// Without a login any local program could reach the VPN's networks.
	if start.SocksUser == "" || start.SocksPass == "" {
		return fail("The start message has no SOCKS login.")
	}
	newEngine, ok := engines[start.Type]
	if !ok {
		return fail(fmt.Sprintf("This hostbay-vpn cannot run VPN type %q.", start.Type))
	}
	eng, err := newEngine(start.Config, start.Secrets)
	if err != nil {
		return fail(err.Error())
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail("Cannot open the local SOCKS port: " + err.Error())
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	go (&socks.Server{User: start.SocksUser, Pass: start.SocksPass, Dial: eng.Dial}).Serve(ln)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// No WireGuard step waits on an answer, so only the end of stdin matters.
		for {
			var m proto.Msg
			if dec.Decode(&m) != nil {
				cancel()
				return
			}
		}
	}()
	eng.Run(ctx, func(m proto.Msg) {
		if m.State == proto.Connected {
			m.SocksPort = port
		}
		send(m)
	})
	return nil
}
