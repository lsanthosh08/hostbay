package agentexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

// fakeHosts is saved SSH hosts as the host gate sees them. Run prints what
// the test says, and blocks while the host's gate channel is set.
type fakeHosts struct {
	mu      sync.Mutex
	ran     []string // host: command, in order
	block   map[string]chan struct{}
	secrets []string
	fail    error
}

func (f *fakeHosts) Label(id string) (string, error) {
	if strings.HasPrefix(id, "h") {
		return "label-" + id, nil
	}
	return "", errors.New("not a saved SSH host")
}
func (f *fakeHosts) Secrets(string) []string { return f.secrets }
func (f *fakeHosts) Run(ctx context.Context, host, cmd string, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.ran = append(f.ran, host+": "+cmd)
	gate, fail := f.block[host], f.fail
	f.mu.Unlock()
	if fail != nil {
		return 0, fail
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	fmt.Fprint(stdout, strings.TrimPrefix(cmd, "echo "))
	fmt.Fprint(stderr, "warn")
	return 0, nil
}
func (f *fakeHosts) List(context.Context, string, string) ([]RemoteEntry, error) {
	return []RemoteEntry{{Name: "a", Path: "/a"}}, nil
}
func (f *fakeHosts) Upload(_ context.Context, _, _ string, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}
func (f *fakeHosts) Download(_ context.Context, _, _ string, w io.Writer) error {
	_, err := io.WriteString(w, "data")
	return err
}

func (f *fakeHosts) runs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

// clock hands out timers the test fires, recording each duration asked for.
type clock struct {
	mu sync.Mutex
	ch map[time.Duration][]chan time.Time
}

func (c *clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.ch[d] = append(c.ch[d], ch)
	return ch
}

// fire ends every timer of d asked for so far, waiting for one to exist.
func (c *clock) fire(t *testing.T, d time.Duration) {
	t.Helper()
	for range 200 {
		c.mu.Lock()
		chs := c.ch[d]
		delete(c.ch, d)
		c.mu.Unlock()
		if len(chs) > 0 {
			for _, ch := range chs {
				ch <- time.Now()
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %v timer was started", d)
}

func newHostRig(t *testing.T) (*HostJobs, *fakeHosts, *clock) {
	t.Helper()
	f := &fakeHosts{block: map[string]chan struct{}{}}
	c := &clock{ch: map[time.Duration][]chan time.Time{}}
	a := NewApprovals()
	a.After = c.After
	h := NewHostJobs(a, f)
	h.After = c.After
	return h, f, c
}

func run(principal, host, cmd string) HostRequest {
	return HostRequest{Principal: principal, Client: "client-" + principal, Host: host, Tool: ToolRun, Command: cmd}
}

// startAsync calls Start and returns its result once it comes back.
func startAsync(h *HostJobs, ctx context.Context, r HostRequest) <-chan HostJobView {
	ch := make(chan HostJobView, 1)
	go func() {
		v, err := h.Start(ctx, r)
		if err != nil {
			v.Error = "ERR " + err.Error()
		}
		ch <- v
	}()
	return ch
}

func pendingFor(t *testing.T, a *Approvals) Pending {
	t.Helper()
	for range 200 {
		if l := a.List(); len(l) > 0 {
			return l[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no request reached the tray")
	return Pending{}
}

func waitState(t *testing.T, h *HostJobs, principal, id string, want JobState) HostJobView {
	t.Helper()
	for range 400 {
		v, err := h.View(principal, id, 0, 0)
		if err == nil && v.State == want {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, err := h.View(principal, id, 0, 0)
	t.Fatalf("job %s is %q (%v), want %q", id, v.State, err, want)
	return v
}

// An approved command runs once on the host and returns its output and
// exit code; the card names the client and the host Go resolved.
func TestHostJobApprovedRuns(t *testing.T) {
	h, f, _ := newHostRig(t)
	res := startAsync(h, context.Background(), run("tokA", "h1", "echo ok"))
	p := pendingFor(t, h.Approvals)
	if p.Client != "client-tokA" || p.Host != "label-h1" || p.Bytes != "echo ok" || p.Tool != ToolRun {
		t.Fatalf("card = %+v", p)
	}
	if len(f.runs()) != 0 {
		t.Fatal("ran before approval")
	}
	if err := h.Approvals.Answer(p.ID, true); err != nil {
		t.Fatal(err)
	}
	v := <-res
	if v.State != JobDone || v.Exit == nil || *v.Exit != 0 || v.Stdout != "ok" || v.Stderr != "warn" {
		t.Fatalf("result = %+v", v)
	}
	if got := f.runs(); len(got) != 1 || got[0] != "h1: echo ok" {
		t.Fatalf("ran %q", got)
	}
}

// A denied request runs nothing; one nobody answers is denied at 110 s.
func TestHostJobDeniedAndExpired(t *testing.T) {
	h, f, c := newHostRig(t)
	res := startAsync(h, context.Background(), run("tokA", "h1", "echo no"))
	h.Approvals.Answer(pendingFor(t, h.Approvals).ID, false)
	if v := <-res; v.State != JobDenied || !strings.Contains(v.Error, "denied") {
		t.Fatalf("denied = %+v", v)
	}
	res = startAsync(h, context.Background(), run("tokA", "h2", "echo late"))
	pendingFor(t, h.Approvals)
	c.fire(t, ApprovalTTL)
	if v := <-res; v.State != JobDenied || !strings.Contains(v.Error, "110 seconds") {
		t.Fatalf("expired = %+v", v)
	}
	if len(f.runs()) != 0 {
		t.Fatalf("ran %q", f.runs())
	}
}

// An unanswered request returns at 20 s with its job ID, awaiting
// approval. Approved later, the job runs and its status says done.
func TestHostJobReturnsAt20Seconds(t *testing.T) {
	h, _, c := newHostRig(t)
	res := startAsync(h, context.Background(), run("tokA", "h1", "echo later"))
	p := pendingFor(t, h.Approvals)
	c.fire(t, 20*time.Second)
	v := <-res
	if v.State != JobAwaiting || v.ID == "" {
		t.Fatalf("at 20 s = %+v", v)
	}
	h.Approvals.Answer(p.ID, true) // at 60 s: the card still waits
	if got := waitState(t, h, "tokA", v.ID, JobDone); got.Stdout != "later" {
		t.Fatalf("status = %+v", got)
	}
}

// Another token cannot see a job.
func TestHostJobBoundToToken(t *testing.T) {
	h, _, _ := newHostRig(t)
	res := startAsync(h, context.Background(), run("tokA", "h1", "echo a"))
	h.Approvals.Answer(pendingFor(t, h.Approvals).ID, true)
	v := <-res
	if _, err := h.View("tokB", v.ID, 0, 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("tokB read tokA's job: %v", err)
	}
	if err := h.Interrupt("tokB", v.ID); err == nil {
		t.Fatal("tokB interrupted tokA's job")
	}
}

// Eight jobs at once and one per host; the next is refused before any card.
func TestHostJobLimits(t *testing.T) {
	h, f, _ := newHostRig(t)
	gate := make(chan struct{})
	f.mu.Lock()
	for i := range 10 {
		f.block[fmt.Sprintf("h%d", i)] = gate
	}
	f.mu.Unlock()
	allow := func(cmd string) []store.AllowEntry { e, _ := ParseEntry(cmd, false); return []store.AllowEntry{e} }
	start := func(tok string, i int) error {
		r := run(tok, fmt.Sprintf("h%d", i), "echo x")
		r.Allow = allow("echo x")
		_, err := h.Start(context.Background(), r)
		return err
	}
	for i := range 8 {
		go start([]string{"tokA", "tokB"}[i%2], i)
	}
	for range 400 {
		if len(f.runs()) == 8 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// One token holds at most its share, so it cannot block every other.
	if err := start("tokA", 8); err == nil || !strings.Contains(err.Error(), "4 of this client's jobs") {
		t.Fatalf("tokA's fifth job: %v", err)
	}
	if err := start("tokC", 9); err == nil || !strings.Contains(err.Error(), "8 jobs") {
		t.Fatalf("ninth job: %v", err)
	}
	r := run("tokB", "h0", "echo x")
	if _, err := h.Start(context.Background(), r); err == nil {
		t.Fatal("a second job on a busy host was accepted")
	}
	if len(h.Approvals.List()) != 0 {
		t.Fatal("a refused job showed a card")
	}
	close(gate)
}

// A token that asks for every slot gets its share, and another token can
// still start a job.
func TestHostJobOneTokenCannotTakeEverySlot(t *testing.T) {
	h, f, _ := newHostRig(t)
	gate := make(chan struct{})
	f.mu.Lock()
	for i := range 9 {
		f.block[fmt.Sprintf("h%d", i)] = gate
	}
	f.mu.Unlock()
	var wg sync.WaitGroup
	for i := range 8 {
		r := run("tokA", fmt.Sprintf("h%d", i), "echo x")
		r.Allow = []store.AllowEntry{{Argv: []string{"echo", "x"}}}
		wg.Go(func() { h.Start(context.Background(), r) })
	}
	for range 400 {
		if len(f.runs()) >= 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let any job past the share start
	r := run("tokB", "h8", "echo x")
	r.Allow = []store.AllowEntry{{Argv: []string{"echo", "x"}}}
	res := startAsync(h, context.Background(), r)
	for range 400 {
		if slices.Contains(f.runs(), "h8: echo x") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gate)
	if v := <-res; v.Error != "" {
		t.Fatalf("tokB after tokA asked for every slot: %s", v.Error)
	}
	wg.Wait()
}

// A job past its run limit ends, and its host is free again.
func TestHostJobRunLimit(t *testing.T) {
	h, f, _ := newHostRig(t)
	h.Limit = 50 * time.Millisecond
	f.block["h1"] = make(chan struct{})
	r := run("tokA", "h1", "sleep 999")
	r.Allow = []store.AllowEntry{{Argv: []string{"sleep", "999"}}}
	v, _ := h.Start(context.Background(), r)
	waitState(t, h, "tokA", v.ID, JobEnded)
}

// Only an exact entry of this token for this host runs without a card.
func TestHostJobAllowlist(t *testing.T) {
	h, f, _ := newHostRig(t)
	e, err := ParseEntry("systemctl status nginx", false)
	if err != nil {
		t.Fatal(err)
	}
	r := run("tokA", "h1", "systemctl status nginx")
	r.Allow = []store.AllowEntry{e}
	if v, _ := h.Start(context.Background(), r); v.State != JobDone {
		t.Fatalf("allowlisted = %+v", v)
	}
	for _, cmd := range []string{"systemctl status nginx; id", "systemctl status nginx extra"} {
		r := run("tokA", "h1", cmd)
		r.Allow = []store.AllowEntry{e}
		res := startAsync(h, context.Background(), r)
		h.Approvals.Answer(pendingFor(t, h.Approvals).ID, false)
		<-res
	}
	res := startAsync(h, context.Background(), run("tokB", "h1", "systemctl status nginx")) // no entries
	h.Approvals.Answer(pendingFor(t, h.Approvals).ID, false)
	<-res
	if got := f.runs(); len(got) != 1 {
		t.Fatalf("ran %q", got)
	}
}

// AutoRead runs a read-only command and a folder listing with no card; a
// command that writes, and a request without AutoRead, still ask.
func TestHostJobAutoReadOnly(t *testing.T) {
	h, f, _ := newHostRig(t)
	r := run("tokA", "h1", "df -h")
	r.AutoRead = true
	if v, _ := h.Start(context.Background(), r); v.State != JobDone {
		t.Fatalf("read-only with AutoRead = %+v", v)
	}
	for name, r := range map[string]HostRequest{
		"writes":     {Principal: "tokA", Host: "h1", Tool: ToolRun, Command: "rm -rf /tmp/x", AutoRead: true},
		"chains":     {Principal: "tokA", Host: "h1", Tool: ToolRun, Command: "df -h; reboot", AutoRead: true},
		"switch off": {Principal: "tokA", Host: "h1", Tool: ToolRun, Command: "df -h"},
		"secret dir": {Principal: "tokA", Host: "h1", Tool: ToolList, Remote: "/home/u/.ssh", AutoRead: true},
		"upload":     {Principal: "tokA", Host: "h1", Tool: ToolUpload, Remote: "/tmp/a", Local: "a", AutoRead: true},
		"download":   {Principal: "tokA", Host: "h1", Tool: ToolDownload, Remote: "/tmp/a", Local: "a", AutoRead: true},
	} {
		res := startAsync(h, context.Background(), r)
		p := pendingFor(t, h.Approvals)
		if p.Bytes != r.bytes() {
			t.Fatalf("%s: card = %q", name, p.Bytes)
		}
		h.Approvals.Answer(p.ID, false)
		<-res
	}
	l := HostRequest{Principal: "tokA", Host: "h1", Tool: ToolList, Remote: "/var/log", AutoRead: true}
	if v, _ := h.Start(context.Background(), l); v.State != JobDone {
		t.Fatalf("listing with AutoRead = %+v", v)
	}
	if got := f.runs(); len(got) != 1 || got[0] != "h1: df -h" {
		t.Fatalf("ran %q", got)
	}
}

// Revoking a token denies its waiting request; its running job finishes,
// and its output is dropped.
func TestHostJobRevoke(t *testing.T) {
	h, f, _ := newHostRig(t)
	gate := make(chan struct{})
	f.block["h1"] = gate
	r := run("tokA", "h1", "echo running")
	r.Allow = []store.AllowEntry{{Argv: []string{"echo", "running"}}}
	runRes := startAsync(h, context.Background(), r)
	for len(f.runs()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	waiting := startAsync(h, context.Background(), run("tokA", "h2", "echo waiting"))
	pendingFor(t, h.Approvals)
	h.Revoke("tokA")
	if v := <-waiting; v.State != JobDenied {
		t.Fatalf("waiting after revoke = %+v", v)
	}
	close(gate)
	v := <-runRes
	if _, err := h.View("tokA", v.ID, 0, 0); err == nil {
		t.Fatal("a revoked token's output is still readable")
	}
	if got := f.runs(); len(got) != 1 {
		t.Fatalf("ran %q", got)
	}
}

// A call whose bearer check passed just before the revoke must not start
// anything once Revoke has run: no auto-run from the old allowlist and no
// new card.
func TestHostJobRevokedTokenStartsNothing(t *testing.T) {
	h, f, _ := newHostRig(t)
	h.Revoke("tokA")
	r := run("tokA", "h1", "echo x")
	r.Allow = []store.AllowEntry{{Argv: []string{"echo", "x"}}}
	if _, err := h.Start(context.Background(), r); err == nil {
		t.Fatal("a revoked token started a job")
	}
	if _, err := h.Start(context.Background(), run("tokA", "h2", "echo y")); err == nil {
		t.Fatal("a revoked token got a card")
	}
	if len(f.runs()) != 0 || len(h.Approvals.List()) != 0 {
		t.Fatalf("ran %q, cards %v", f.runs(), h.Approvals.List())
	}
	if v, err := h.Start(context.Background(), r); err == nil || v.ID != "" {
		t.Fatal("still accepted")
	}
	r.Principal = "tokB"
	if v, err := h.Start(context.Background(), r); err != nil || v.State != JobDone {
		t.Fatalf("another token: %+v, %v", v, err)
	}
}

// A job registered, or approved, just before Revoke but not yet sent to
// the host never reaches it, and ends rather than holding its slot.
func TestHostJobRevokedBeforeItBeginsNeverRuns(t *testing.T) {
	h, f, _ := newHostRig(t)
	j := &HostJob{ID: newID(), Principal: "tokA", Host: "h1", state: JobRunning, done: make(chan struct{}), cancelAsk: func() {}}
	if err := h.add(j); err != nil {
		t.Fatal(err)
	}
	h.Revoke("tokA")
	h.run(j, run("tokA", "h1", "echo x"))
	if got := f.runs(); len(got) != 0 {
		t.Fatalf("ran %q after the revoke", got)
	}
	if j.active() {
		t.Fatal("the revoked job still holds its slot")
	}
}

// A client gone before the result withdraws its card; nothing runs.
func TestHostJobClientGone(t *testing.T) {
	h, f, _ := newHostRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	res := startAsync(h, ctx, run("tokA", "h1", "echo gone"))
	pendingFor(t, h.Approvals)
	cancel()
	<-res
	for range 200 {
		if len(h.Approvals.List()) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(h.Approvals.List()) != 0 || len(f.runs()) != 0 {
		t.Fatalf("tray %v, ran %q", h.Approvals.List(), f.runs())
	}
}

// Output and errors never carry a host's secret, even split across reads,
// and a refused proposal names nothing secret.
func TestHostJobScrubsSecrets(t *testing.T) {
	h, f, _ := newHostRig(t)
	f.secrets = []string{"hunter2pw"}
	r := run("tokA", "h1", "echo pre hunter2pw post")
	r.Allow = []store.AllowEntry{{Argv: []string{"echo", "pre", "hunter2pw", "post"}}}
	v, _ := h.Start(context.Background(), r)
	if strings.Contains(v.Stdout, "hunter2pw") || !strings.Contains(v.Stdout, "[secret removed]") {
		t.Fatalf("stdout = %q", v.Stdout)
	}
	for off := range len(v.Stdout) {
		p, _ := h.View("tokA", v.ID, off, 0)
		if strings.Contains(p.Stdout, "hunter") && strings.Contains(p.Stdout, "2pw") {
			t.Fatalf("read at %d = %q", off, p.Stdout)
		}
	}
	f.fail = apperr.New(apperr.SecretNeeded, "no password for hunter2pw")
	r = run("tokA", "h2", "id")
	r.Allow = []store.AllowEntry{{Argv: []string{"id"}}}
	if v, _ := h.Start(context.Background(), r); v.State != JobFailed || v.Code != string(apperr.SecretNeeded) || strings.Contains(v.Error, "hunter2pw") {
		t.Fatalf("failed = %+v", v)
	}
}

// A running job holds back the tail a secret could start in.
func TestHostJobHoldsBackPartialSecret(t *testing.T) {
	j := &HostJob{state: JobRunning, done: make(chan struct{})}
	j.out = []byte("abc hunt")
	if got := j.view([]string{"hunter2pw"}, 0, 0).Stdout; strings.Contains(got, "hunt") {
		t.Fatalf("running view = %q", got)
	}
}

// Requests naming another kind of host, or holding control bytes, are
// refused before any card.
func TestHostJobRefusals(t *testing.T) {
	h, _, _ := newHostRig(t)
	for _, r := range []HostRequest{run("tokA", "local", "id"), run("tokA", "h1", "id\nrm -rf /"), run("tokA", "h1", " ")} {
		if _, err := h.Start(context.Background(), r); err == nil {
			t.Errorf("%+v was accepted", r)
		}
	}
	if len(h.Approvals.List()) != 0 {
		t.Fatal("a refused request showed a card")
	}
}

// A finished job's output is dropped 10 minutes after it ended, even when
// no other job starts.
func TestHostJobRetention(t *testing.T) {
	h, _, _ := newHostRig(t)
	now := time.Now()
	h.Now = func() time.Time { return now }
	r := run("tokA", "h1", "echo old")
	r.Allow = []store.AllowEntry{{Argv: []string{"echo", "old"}}}
	v, _ := h.Start(context.Background(), r)
	if _, err := h.View("tokA", v.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(jobRetention + time.Second)
	if _, err := h.View("tokA", v.ID, 0, 0); err == nil {
		t.Fatal("output kept past retention")
	}
}
