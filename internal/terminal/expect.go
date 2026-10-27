package terminal

import (
	"cmp"
	"errors"
	"strings"
	"time"
)

// Post-login steps on the tap (KTD39), after the shape of uniterm's
// post_login_expect.go step cursor. Unlike uniterm, a step's expected text
// matches only at the end of the last unterminated output line, in output
// newer than the previous step, so text in a banner is not a prompt.

var (
	ErrExpectTimeout = errors.New("the expected prompt did not appear in time, so the post-login rule stopped and sent nothing more")
	ErrExpectTyped   = errors.New("you typed before the prompt, so the post-login rule did not send its secret")
	ErrExpectEnded   = errors.New("the terminal ended before the post-login rule finished")
)

const (
	expectTimeout = 10 * time.Second
	expectQuiet   = 300 * time.Millisecond
	// A match must hold this long with no more output: a banner that
	// arrives split mid-sentence is not a prompt waiting for an answer.
	expectSettle = 100 * time.Millisecond
)

// ExpectStep waits for Expect, or for quiet when it is empty. Secret marks
// a reply that may be sent only before the user's first keystroke.
type ExpectStep struct {
	Expect string
	Secret bool
}

// Expect runs Steps in order on one terminal's output.
type Expect struct {
	Steps   []ExpectStep
	Timeout time.Duration // per step; 10 seconds when 0
	Typed   func() bool   // whether the user has typed in the terminal
	Send    func(i int) error
}

// Run waits for each step in turn and calls Send for it, once. It stops
// at a step that times out, a secret step after the user typed, or the
// end of the terminal. The caller closes sub.
func (x Expect) Run(sub *Subscription) error {
	timeout := cmp.Or(x.Timeout, expectTimeout)
	for i, st := range x.Steps {
		if err := wait(sub, strings.TrimRight(st.Expect, " \t"), timeout); err != nil {
			return err
		}
		if st.Secret && x.Typed != nil && x.Typed() {
			return ErrExpectTyped
		}
		if err := x.Send(i); err != nil {
			return err
		}
	}
	return nil
}

// wait returns once want ends the last unterminated line of the output
// that arrives from now on, or once that output has been quiet when want
// is empty.
func wait(sub *Subscription, want string, timeout time.Duration) error {
	var lp lines
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ready := time.NewTimer(expectQuiet)
	defer ready.Stop()
	if want != "" {
		ready.Stop()
	}
	for {
		select {
		case chunk, ok := <-sub.C:
			if !ok {
				return ErrExpectEnded
			}
			lp.feed(chunk, func(string) {})
			switch {
			case want == "":
				ready.Reset(expectQuiet)
			case strings.HasSuffix(strings.TrimRight(lp.current(), " \t"), want):
				ready.Reset(expectSettle)
			default:
				ready.Stop()
			}
		case <-ready.C:
			return nil
		case <-deadline.C:
			return ErrExpectTimeout
		}
	}
}
