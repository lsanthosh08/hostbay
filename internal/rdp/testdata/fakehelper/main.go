// Command fakehelper stands in for rdp-helper in the supervisor tests. It
// speaks the same stdin/stdout protocol, and FAKE_RDP_MODE picks a
// misbehaviour.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

var out = bufio.NewWriter(os.Stdout)

func send(v map[string]any) {
	b, _ := json.Marshal(v)
	out.Write(append(b, '\n'))
	out.Flush()
}

func frame(x, y, w, h int, fill byte) {
	px := make([]byte, w*h*4)
	for i := range px {
		px[i] = fill
	}
	b, _ := json.Marshal(map[string]any{"t": "frame", "x": x, "y": y, "w": w, "h": h, "len": len(px)})
	out.Write(append(b, '\n'))
	out.Write(px)
	out.Flush()
}

func main() {
	mode := os.Getenv("FAKE_RDP_MODE")
	switch mode {
	case "silent":
		time.Sleep(time.Minute)
		return
	case "badversion":
		send(map[string]any{"t": "hello", "version": 99})
		time.Sleep(time.Minute)
		return
	}
	send(map[string]any{"t": "hello", "version": 1})

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	var connect map[string]any
	if !in.Scan() || json.Unmarshal(in.Bytes(), &connect) != nil || connect["t"] != "connect" {
		os.Exit(2)
	}
	if mode == "fail" {
		send(map[string]any{"t": "error", "message": "connect finalize (auth): logon failure"})
		os.Exit(1)
	}

	send(map[string]any{"t": "cert", "der": "ZmFrZS1jZXJ0"}) // "fake-cert"
	var trust map[string]any
	if !in.Scan() || json.Unmarshal(in.Bytes(), &trust) != nil || trust["t"] != "trust" {
		os.Exit(2)
	}
	if trust["accept"] != true {
		os.Exit(3)
	}

	if mode == "big" { // full-screen frames, more bytes than the page queue holds
		send(map[string]any{"t": "size", "width": 1920, "height": 1080})
		for i := range 20 {
			frame(0, 0, 1920, 1080, byte(i))
		}
		return
	}
	send(map[string]any{"t": "size", "width": 4, "height": 2})
	frame(0, 0, 4, 2, 0xAA)
	frame(1, 1, 1, 1, 0xBB)
	// Report what this process was given, so the test can look for the
	// password in its arguments and environment.
	seen, _ := json.Marshal(map[string]any{
		"args": strings.Join(os.Args, " "), "env": strings.Join(os.Environ(), "\n"), "password": connect["password"],
	})
	send(map[string]any{"t": "clipboardText", "text": string(seen)})

	switch mode {
	case "crash":
		os.Exit(7)
	case "badframe":
		send(map[string]any{"t": "frame", "x": 3, "y": 1, "w": 4, "h": 2, "len": 32})
		time.Sleep(time.Minute)
		return
	}
	// Echo every later message back as clipboard text until told to close.
	for in.Scan() {
		line := in.Text()
		if strings.Contains(line, `"t":"close"`) {
			return
		}
		send(map[string]any{"t": "clipboardText", "text": line})
	}
	fmt.Fprintln(os.Stderr, "fakehelper: stdin closed")
}
