package claudecode

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hostbay/internal/ai"
	"hostbay/internal/apperr"
)

// MinVersion is the oldest Claude Code this was built and tested against
// (testdata/FINDINGS.md). The startup probe is the real detector: a version
// that rejects a flag exits with "unknown option".
const MinVersion = "2.1.308"

// Models are the aliases Claude Code takes; no network call lists them.
var Models = ai.DefaultModels[ai.ClaudeCode]

// defaultModel answers a request that names none; testModel is the cheapest,
// for the one call Test makes.
const (
	defaultModel = "sonnet"
	testModel    = "haiku"
)

// ValidModel reports whether name can be given to the program as a model.
func ValidModel(name string) bool { return modelRE.MatchString(name) }

var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\[\]-]{0,63}$`)

// Provider is the client for one Claude Code program.
type Provider struct {
	// Path is the program as the person selected it (see Rules.Check).
	Path string

	rules   Rules
	environ func() []string

	tagFn func() string // newTag; a test fixes it

	mu      sync.Mutex
	checked string // the version the last successful Test saw
}

// New returns the provider for the program at path.
func New(path string) *Provider {
	return &Provider{Path: path, rules: DefaultRules(), environ: os.Environ, tagFn: newTag}
}

// Info is what setup shows about a program.
type Info struct {
	Path, Target string
	Version      string
	Status       Status
}

func (p *Provider) env() []string { return Env(p.environ(), p.rules.GOOS) }

// Models returns the aliases.
func (p *Provider) Models(context.Context) ([]string, error) {
	return append([]string(nil), Models...), nil
}

// Inspect runs `claude --version` and `claude auth status` for the selected
// program, in the scrubbed environment, and returns what they say. It makes
// no model call. A program that is too old or not signed in is still
// described, with the error that says why it cannot be used.
func (p *Provider) Inspect(ctx context.Context) (Info, error) {
	res, err := p.rules.Check(p.Path)
	if err != nil {
		return Info{}, errMissing
	}
	info := Info{Path: res.Path, Target: res.Target}
	env := p.env()
	out, err := run(ctx, res.Target, []string{"--version"}, env)
	if err != nil || out == "" {
		return info, errMissing
	}
	info.Version = parseVersion(out)
	if info.Version == "" || semverLess(parseSemver(info.Version), parseSemver(MinVersion)) {
		return info, errTooOld
	}
	out, err = run(ctx, res.Target, []string{"auth", "status"}, env)
	if err != nil && out == "" { // a signed-out program may exit non-zero with the JSON
		return info, errChanged
	}
	info.Status, err = parseStatus(out)
	if err != nil {
		return info, errChanged
	}
	switch {
	case !info.Status.LoggedIn:
		return info, errSignedOut
	case !info.Status.firstParty():
		return info, errOtherAPI
	}
	return info, nil
}

// SetChecked tells a new Provider for the same program which version the
// last successful Test saw, so a later change is reported (KTD8).
func (p *Provider) SetChecked(version string) {
	p.mu.Lock()
	p.checked = version
	p.mu.Unlock()
}

// Test is Inspect followed by one small real call, so a flag the program
// rejects shows now, not in the person's first message.
func (p *Provider) Test(ctx context.Context) (Info, error) {
	info, err := p.Inspect(ctx)
	if err != nil {
		return info, err
	}
	_, err = p.Stream(ctx, ai.Request{
		Model:    testModel,
		System:   "Answer in one word.",
		Messages: []ai.Message{{Role: "user", Content: []ai.Block{{Type: "text", Text: "Reply with exactly the word: ok"}}}},
	}, func(string) {})
	if err == nil {
		p.SetChecked(info.Version)
	}
	return info, err
}

func parseVersion(out string) string {
	return semverRE.FindString(out)
}

func semverLess(a, b semver) bool { return b.newer(a) }

// Stream runs one model call (KTD3). It renders Hostbay's conversation into
// one prompt, runs the program, and returns the reply with any tool calls
// read from tagged blocks. A reply holding an unreadable block is asked for
// once more inside this call (KTD4).
func (p *Provider) Stream(ctx context.Context, r ai.Request, text func(string)) (ai.Reply, error) {
	model := r.Model
	if model == "" {
		model = defaultModel
	}
	if !ValidModel(model) {
		return ai.Reply{}, apperr.New(apperr.ClaudeCodeStopped, "That model name is not one Claude Code can take.")
	}
	tag := p.tagFn()
	var sent strings.Builder
	for attempt := 0; ; attempt++ {
		retry := attempt > 0
		skip := newPrefixSkipper(sent.String())
		emit := func(s string) {
			if out := skip.filter(s); out != "" {
				sent.WriteString(out)
				text(out)
			}
		}
		bp := newBlockParser(tag, r.Tools, retry, emit)
		if err := p.call(ctx, model, render(r, tag, retry), bp); err != nil {
			return ai.Reply{}, err
		}
		if bp.bad > 0 && !retry {
			continue
		}
		msg := ai.Message{Role: "assistant"}
		// What the page showed is what history keeps, so the two never differ.
		if v := sent.String(); strings.TrimSpace(v) != "" {
			msg.Content = append(msg.Content, ai.Block{Type: "text", Text: v})
		}
		msg.Content = append(msg.Content, bp.calls...)
		stop := "end_turn"
		if len(bp.calls) > 0 {
			stop = "tool_use"
		}
		return ai.Reply{Message: msg, StopReason: stop}, nil
	}
}

// call runs the program once and feeds its text to bp. It returns an error
// for anything but a finished answer, and never a partial one.
func (p *Provider) call(ctx context.Context, model, prompt string, bp *blockParser) error {
	res, err := p.rules.Check(p.Path)
	if err != nil {
		return errMissing
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idled atomic.Bool
	idle := time.AfterFunc(idleTimeout, func() { idled.Store(true); cancel() })
	defer idle.Stop()

	proc, out, cleanup, err := start(callCtx, res.Target, modelArgs(model), prompt, p.env())
	if err != nil {
		return errMissing
	}
	defer cleanup()
	var st streamState
	readErr := readStream(out, &st, bp.write, func() { idle.Reset(idleTimeout) })
	if readErr != nil {
		cancel() // end the program before waiting for it
	}
	exit := proc.wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case idled.Load():
		return errIdle
	case readErr != nil:
		var ae *apperr.Error
		if errors.As(readErr, &ae) {
			return ae
		}
		return stopped(exit)
	}
	if fail := st.failure(exit, proc.stderr.b.String()); fail != nil {
		return p.classify(ctx, res.Target, fail)
	}
	if !st.gotDelta && st.finalText != "" { // partial messages were not sent
		bp.write(st.finalText)
	}
	bp.flush()
	return nil
}

// classify turns a sign-in or flag failure into "update Hostbay" when the
// program is signed in or has changed version since the last test (KTD8).
func (p *Provider) classify(ctx context.Context, target string, fail *apperr.Error) error {
	if fail.Code != apperr.ClaudeCodeSignedOut && fail.Code != apperr.ClaudeCodeTooOld {
		return fail
	}
	p.mu.Lock()
	checked := p.checked
	p.mu.Unlock()
	env := p.env()
	if checked != "" {
		if out, err := run(ctx, target, []string{"--version"}, env); err == nil && parseVersion(out) != checked {
			return errChanged
		}
	}
	if fail.Code == apperr.ClaudeCodeSignedOut {
		if out, _ := run(ctx, target, []string{"auth", "status"}, env); out != "" {
			if s, err := parseStatus(out); err == nil && s.LoggedIn && s.firstParty() {
				return errChanged
			}
		}
	}
	return fail
}

// prefixSkipper drops the start of a second try's text that repeats what the
// first try already sent, so the page does not show the same words twice. A
// second try that words it differently is shown whole, after a break, because
// the page can only append.
type prefixSkipper struct {
	already string
	pos     int
	done    bool
}

func newPrefixSkipper(already string) *prefixSkipper { return &prefixSkipper{already: already} }

func (k *prefixSkipper) filter(s string) string {
	if k.done || k.already == "" {
		return s
	}
	start := k.pos
	for i := 0; i < len(s); i++ {
		if k.pos >= len(k.already) {
			k.done = true
			return s[i:]
		}
		if s[i] != k.already[k.pos] {
			k.done = true
			return "\n\n" + k.already[:start] + s
		}
		k.pos++
	}
	return ""
}
