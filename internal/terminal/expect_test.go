package terminal

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// expectRun runs steps on a fresh terminal and returns what was sent and
// how the run ended. say plays the remote, before the run ends.
func expectRun(t *testing.T, steps []ExpectStep, timeout time.Duration, typed func() bool, say func(remote func(string))) ([]int, error) {
	t.Helper()
	r, _ := newLoggedRegistry()
	p := newPipe()
	id := r.Open("s", p)
	sub, _ := r.Subscribe(id)
	var mu sync.Mutex
	var sent []int
	done := make(chan error, 1)
	go func() {
		done <- Expect{Steps: steps, Timeout: timeout, Typed: typed, Send: func(i int) error {
			mu.Lock()
			sent = append(sent, i)
			mu.Unlock()
			return nil
		}}.Run(sub)
	}()
	say(func(s string) { p.remote.Write([]byte(s)) })
	err := <-done
	r.Close(id)
	mu.Lock()
	defer mu.Unlock()
	return sent, err
}

func never() bool { return false }

func TestExpectRepliesOnceWhenThePromptEndsTheLastLine(t *testing.T) {
	sent, err := expectRun(t, []ExpectStep{{Expect: "Password:", Secret: true}}, time.Second, never, func(say func(string)) {
		say("Last login: today\r\n\x1b[1mPassword: \x1b[0m")
	})
	if err != nil || len(sent) != 1 || sent[0] != 0 {
		t.Fatalf("sent %v, %v", sent, err)
	}
}

// Text in the middle of a line, even one whose end arrives in a later
// chunk, is not a prompt.
func TestExpectIgnoresTheTextMidSentence(t *testing.T) {
	sent, err := expectRun(t, []ExpectStep{{Expect: "password:", Secret: true}}, 600*time.Millisecond, never, func(say func(string)) {
		say("Reminder: your password: expires soon\r\n")
		say("Change your password:")
		say(" with passwd\r\n$ ")
	})
	if !errors.Is(err, ErrExpectTimeout) || len(sent) != 0 {
		t.Fatalf("sent %v, %v", sent, err)
	}
}

func TestExpectSendsNoSecretAfterTheUserTyped(t *testing.T) {
	sent, err := expectRun(t, []ExpectStep{{Expect: "Password:", Secret: true}}, time.Second, func() bool { return true }, func(say func(string)) {
		say("Password: ")
	})
	if !errors.Is(err, ErrExpectTyped) || len(sent) != 0 {
		t.Fatalf("sent %v, %v", sent, err)
	}
}

func TestExpectTimesOutAndSendsNothing(t *testing.T) {
	start := time.Now()
	sent, err := expectRun(t, []ExpectStep{{Expect: "Password:"}}, 300*time.Millisecond, never, func(say func(string)) { say("$ ") })
	if !errors.Is(err, ErrExpectTimeout) || len(sent) != 0 {
		t.Fatalf("sent %v, %v", sent, err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("timed out early")
	}
}

// A step with no expected text waits for quiet; the next step matches only
// output that came after the previous reply.
func TestExpectStepsRunInOrderOnNewerOutput(t *testing.T) {
	steps := []ExpectStep{{}, {Expect: "Password:"}}
	sent, err := expectRun(t, steps, 2*time.Second, never, func(say func(string)) {
		say("Password: ") // before the first step's quiet: not the second step's prompt
		time.Sleep(450 * time.Millisecond)
		say("\r\n$ su\r\nPassword: ")
	})
	if err != nil || len(sent) != 2 {
		t.Fatalf("sent %v, %v", sent, err)
	}
}

func TestExpectEndsWithTheTerminal(t *testing.T) {
	r, _ := newLoggedRegistry()
	p := newPipe()
	id := r.Open("s", p)
	sub, _ := r.Subscribe(id)
	go r.Close(id)
	err := Expect{Steps: []ExpectStep{{Expect: "x"}}, Send: func(int) error { t.Error("sent"); return nil }}.Run(sub)
	if !errors.Is(err, ErrExpectEnded) {
		t.Fatalf("err = %v", err)
	}
}
