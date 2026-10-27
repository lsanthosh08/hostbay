package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

// HostExec is saved SSH hosts as MCP reaches them (KTD36): a non-PTY exec
// and SFTP on each host's KTD5 lease, dialled as part of an approved
// request. Nothing in it prompts: a host key or secret it lacks comes back
// as HostKeyUnknown, HostKeyChanged or SecretNeeded.
type HostExec interface {
	Label(hostID string) (string, error) // a saved SSH host's label; any other host is an error
	Secrets(hostID string) []string      // what must never reach the client
	Run(ctx context.Context, hostID, command string, stdout, stderr io.Writer) (int, error)
	List(ctx context.Context, hostID, dir string) ([]RemoteEntry, error)
	Upload(ctx context.Context, hostID, remote string, r io.Reader) error
	Download(ctx context.Context, hostID, remote string, w io.Writer) error
}

// RemoteEntry is one entry of a remote folder.
type RemoteEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  uint64 `json:"size"`
	Mtime int64  `json:"mtime"`
}

// The host tools (KTD36).
const (
	ToolRun      = "run_command"
	ToolList     = "list_remote_dir"
	ToolUpload   = "upload_file"
	ToolDownload = "download_file"
)

// Job states only a host job reaches.
const (
	JobAwaiting JobState = "awaiting_approval"
	JobDenied   JobState = "denied" // denied, expired, withdrawn or revoked; nothing ran
	JobFailed   JobState = "failed" // the host could not be reached or refused the action
)

// Limits on host jobs, from uniterm's backend/mcp/job.go (KTD36).
const (
	maxHostJobs = 8
	// maxTokenJobs is one token's share of maxHostJobs, so one client
	// cannot hold every slot and block the others.
	maxTokenJobs = maxHostJobs / 2
	hostRunWait  = 20 * time.Second
	hostRunMax   = 5 * time.Minute
)

// HostRequest is one MCP tool call that touches a host.
type HostRequest struct {
	Principal string // "mcp:" and the token's ID
	Client    string // the token's name, which the card shows
	Host      string // the saved host's ID
	Tool      string
	Command   string             // run_command: exactly what runs
	Remote    string             // the remote folder or file
	Local     string             // the file in the MCP folder, as the card shows it
	Allow     []store.AllowEntry // the principal's exact entries for Host; run_command only
	// Open reads the local file of an upload; Create makes the local file of
	// a download, and discard removes it if the download fails. Neither is
	// called before the request is approved.
	Open   func() (io.ReadCloser, error)
	Create func() (w io.WriteCloser, discard func(), err error)
}

// bytes is what the card shows and what runs.
func (r HostRequest) bytes() string {
	switch r.Tool {
	case ToolList:
		return "list " + r.Remote
	case ToolUpload:
		return "upload " + r.Local + " to " + r.Remote
	case ToolDownload:
		return "download " + r.Remote + " to " + r.Local
	}
	return r.Command
}

// HostJobs gates and runs MCP requests on saved hosts. It shares the
// app-wide tray with the agent's Executor; it has no modes and no bypass,
// and only the token's exact entries for the host run without a card.
type HostJobs struct {
	Approvals *Approvals
	Wait      time.Duration // how long Start waits, for approval and then the job
	Limit     time.Duration // a running job ends here
	Now       func() time.Time
	After     func(time.Duration) <-chan time.Time

	hosts   HostExec
	mu      sync.Mutex
	jobs    map[string]*HostJob
	revoked map[string]bool // principals Revoke has seen; they start nothing
}

func NewHostJobs(a *Approvals, hosts HostExec) *HostJobs {
	return &HostJobs{Approvals: a, Wait: hostRunWait, Limit: hostRunMax, Now: time.Now, After: time.After, hosts: hosts, jobs: map[string]*HostJob{}, revoked: map[string]bool{}}
}

// HostJob is one MCP request on a host.
type HostJob struct {
	ID, Principal, Host string

	mu        sync.Mutex
	state     JobState
	exit      int
	err       error
	out, eout []byte
	truncated bool
	dropped   bool // its token was revoked: its output is gone
	ended     time.Time
	cancel    context.CancelFunc
	cancelAsk context.CancelFunc // withdraws its card
	done      chan struct{}
}

// HostJobView is a host job's state and a slice of its output, scrubbed.
type HostJobView struct {
	ID         string   `json:"jobId"`
	State      JobState `json:"state"`
	Exit       *int     `json:"exitCode,omitempty"`
	Stdout     string   `json:"stdout"`
	Stderr     string   `json:"stderr"`
	Offset     int      `json:"offset"`     // where Stdout starts
	Next       int      `json:"next"`       // the stdout offset to read from next
	StderrNext int      `json:"stderrNext"` // the stderr offset to read from next
	Truncated  bool     `json:"truncated,omitempty"`
	Error      string   `json:"error,omitempty"`
	Code       string   `json:"code,omitempty"` // the error's code, such as HostKeyUnknown
}

var (
	errRevoked     = errors.New("the client's token was revoked; nothing ran")
	errJobNotFound = errors.New("job not found")
)

// Start gates r and runs it once allowed. It returns within Wait with the
// job as it stands: awaiting approval, running, or ended. The card keeps
// waiting after that, and View reports the outcome. If ctx ends before
// Start returns, the client is gone and its card is withdrawn.
func (h *HostJobs) Start(ctx context.Context, r HostRequest) (HostJobView, error) {
	if err := CheckBytes(r.Command + r.Remote + r.Local); err != nil {
		return HostJobView{}, &Refusal{err.Error()}
	}
	if r.Tool == ToolRun && strings.TrimSpace(r.Command) == "" {
		return HostJobView{}, &Refusal{"the command is empty"}
	}
	label, err := h.hosts.Label(r.Host)
	if err != nil {
		return HostJobView{}, &Refusal{err.Error()}
	}
	auto := r.Tool == ToolRun && Match(r.Command, r.Allow)
	j := &HostJob{ID: newID(), Principal: r.Principal, Host: r.Host, state: JobAwaiting, done: make(chan struct{})}
	if auto {
		j.state = JobRunning
	}
	askCtx, cancelAsk := context.WithCancel(context.Background())
	j.cancelAsk = cancelAsk
	if err := h.add(j); err != nil {
		cancelAsk()
		return HostJobView{}, err
	}
	stop := context.AfterFunc(ctx, cancelAsk)
	go func() {
		defer cancelAsk()
		if !auto {
			p := Pending{Principal: r.Principal, Client: r.Client, Target: r.Host, Host: label, Tool: r.Tool, Bytes: r.bytes()}
			if _, err := h.Approvals.ask(askCtx, p); err != nil {
				if errors.Is(err, context.Canceled) {
					err = errWithdrawn
					if j.isDropped() {
						err = errRevoked
					}
				}
				j.finish(JobDenied, 0, err, h.Now())
				return
			}
		}
		h.run(j, r)
	}()
	select {
	case <-j.done:
	case <-h.After(h.Wait):
	case <-ctx.Done():
		cancelAsk()
	}
	stop() // from here the card belongs to the job, not to this call
	return j.view(h.hosts.Secrets(r.Host), 0, 0), nil
}

// add registers j unless its principal was revoked, 8 jobs or 4 of its
// principal's are already waiting or running, or one is on j's host.
func (h *HostJobs) add(j *HostJob) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.revoked[j.Principal] {
		return &Refusal{errRevoked.Error()}
	}
	h.prune()
	active, mine := 0, 0
	for _, o := range h.jobs {
		if !o.active() {
			continue
		}
		if o.Host == j.Host {
			return &Refusal{"a job is already waiting or running on this host; wait for it, check job_status or interrupt it"}
		}
		active++
		if o.Principal == j.Principal {
			mine++
		}
	}
	if mine >= maxTokenJobs {
		return &Refusal{fmt.Sprintf("%d of this client's jobs are already waiting or running; wait for one to finish", maxTokenJobs)}
	}
	if active >= maxHostJobs {
		return &Refusal{fmt.Sprintf("%d jobs are already waiting or running; wait for one to finish", maxHostJobs)}
	}
	h.jobs[j.ID] = j
	return nil
}

// prune forgets jobs that ended more than jobRetention ago; h.mu is held.
func (h *HostJobs) prune() {
	for id, j := range h.jobs {
		j.mu.Lock()
		old := !j.ended.IsZero() && h.Now().Sub(j.ended) > jobRetention
		j.mu.Unlock()
		if old {
			delete(h.jobs, id)
		}
	}
}

// run carries out an allowed request within Limit.
func (h *HostJobs) run(j *HostJob, r HostRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), h.Limit)
	defer cancel()
	if !j.begin(cancel) {
		j.finish(JobDenied, 0, errRevoked, h.Now()) // a no-op unless revoked
		return
	}
	stdout, stderr := jobWriter{j, false}, jobWriter{j, true}
	var code int
	var err error
	switch r.Tool {
	case ToolRun:
		code, err = h.hosts.Run(ctx, r.Host, r.Command, stdout, stderr)
	case ToolList:
		var entries []RemoteEntry
		if entries, err = h.hosts.List(ctx, r.Host, r.Remote); err == nil {
			err = json.NewEncoder(stdout).Encode(entries)
		}
	case ToolUpload:
		var f io.ReadCloser
		if f, err = r.Open(); err == nil {
			err = h.hosts.Upload(ctx, r.Host, r.Remote, f)
			f.Close()
		}
		if err == nil {
			fmt.Fprintf(stdout, "uploaded %s to %s", r.Local, r.Remote)
		}
	case ToolDownload:
		err = download(ctx, h.hosts, r)
		if err == nil {
			fmt.Fprintf(stdout, "downloaded %s to %s", r.Remote, r.Local)
		}
	default:
		err = fmt.Errorf("unknown tool %q", r.Tool)
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		j.finish(JobEnded, 0, fmt.Errorf("the job reached its %v limit and was stopped", h.Limit), h.Now())
	case err != nil:
		j.finish(JobFailed, 0, err, h.Now())
	default:
		j.finish(JobDone, code, nil, h.Now())
	}
}

func download(ctx context.Context, hosts HostExec, r HostRequest) error {
	w, discard, err := r.Create()
	if err != nil {
		return err
	}
	err = hosts.Download(ctx, r.Host, r.Remote, w)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		discard()
	}
	return err
}

func (h *HostJobs) job(principal, id string) (*HostJob, error) {
	h.mu.Lock()
	h.prune()
	j := h.jobs[id]
	h.mu.Unlock()
	if j == nil || j.Principal != principal {
		return nil, errJobNotFound
	}
	j.mu.Lock()
	dropped := j.dropped
	j.mu.Unlock()
	if dropped {
		return nil, errJobNotFound
	}
	return j, nil
}

// View reads principal's own job from the stdout and stderr offsets;
// another token's job is not found.
func (h *HostJobs) View(principal, id string, offset, errOffset int) (HostJobView, error) {
	j, err := h.job(principal, id)
	if err != nil {
		return HostJobView{}, err
	}
	return j.view(h.hosts.Secrets(j.Host), max(0, offset), max(0, errOffset)), nil
}

// Interrupt stops principal's own job: a running one is cancelled, a
// waiting one is withdrawn from the tray.
func (h *HostJobs) Interrupt(principal, id string) error {
	j, err := h.job(principal, id)
	if err != nil {
		return err
	}
	h.Approvals.drop(func(p Pending) bool { return p.Principal == principal && p.Target == j.Host }, errWithdrawn)
	j.mu.Lock()
	cancel := j.cancel
	j.mu.Unlock()
	if j.finish(JobInterrupted, 0, nil, h.Now()) && cancel != nil {
		cancel()
	}
	return nil
}

// Revoke denies principal's waiting requests and drops its jobs' output;
// its running jobs finish (KTD36). From here on it starts nothing, so a
// call that passed the token check just before cannot add a job or a card.
func (h *HostJobs) Revoke(principal string) {
	h.mu.Lock()
	h.revoked[principal] = true
	for _, j := range h.jobs {
		if j.Principal == principal {
			j.mu.Lock()
			j.dropped, j.out, j.eout = true, nil, nil
			j.mu.Unlock()
			j.cancelAsk()
		}
	}
	h.mu.Unlock()
	h.Approvals.drop(func(p Pending) bool { return p.Principal == principal }, errRevoked)
}

func (j *HostJob) isDropped() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dropped
}

// active reports a job waiting for its card or running.
func (j *HostJob) active() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state == JobAwaiting || j.state == JobRunning
}

// begin marks an approved job running; false if it already ended or its
// token was revoked before it touched the host.
func (j *HostJob) begin(cancel context.CancelFunc) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.dropped || j.state != JobAwaiting && j.state != JobRunning {
		return false
	}
	j.state, j.cancel = JobRunning, cancel
	return true
}

// finish ends a job once; it reports whether this call ended it.
func (j *HostJob) finish(state JobState, exit int, err error, now time.Time) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != JobAwaiting && j.state != JobRunning {
		return false
	}
	j.state, j.exit, j.err, j.ended = state, exit, err, now
	close(j.done)
	return true
}

type jobWriter struct {
	j      *HostJob
	stderr bool
}

func (w jobWriter) Write(p []byte) (int, error) {
	j := w.j
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.dropped {
		return len(p), nil
	}
	buf := &j.out
	if w.stderr {
		buf = &j.eout
	}
	q := p
	if room := maxJobOutput - len(*buf); room < len(q) {
		q, j.truncated = q[:max(0, room)], true
	}
	*buf = append(*buf, q...)
	return len(p), nil
}

// view scrubs the job's whole output and returns up to readChunk of each
// stream from its offset. While the job runs, the last bytes a secret
// could start in are held back, so no read shows part of one.
func (j *HostJob) view(secrets []string, offset, errOffset int) HostJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	hold := 0
	if j.state == JobAwaiting || j.state == JobRunning {
		for _, s := range secrets {
			hold = max(hold, len(s)-1)
		}
	}
	v := HostJobView{ID: j.ID, State: j.state, Truncated: j.truncated, Offset: offset}
	v.Stdout, v.Next = slice(Scrub(string(j.out), secrets), offset, hold)
	v.Stderr, v.StderrNext = slice(Scrub(string(j.eout), secrets), errOffset, hold)
	if j.state == JobDone {
		v.Exit = new(j.exit)
	}
	if j.err != nil {
		v.Error, v.Code = scrub(j.err.Error(), secrets), string(apperr.CodeOf(j.err))
	}
	return v
}

// slice is up to readChunk of s from offset, leaving out its last hold
// bytes and ending on a character boundary; it returns where it ended.
func slice(s string, offset, hold int) (string, int) {
	end := max(0, len(s)-hold)
	offset = min(offset, end)
	end = min(end, offset+readChunk)
	for end > offset && end < len(s) && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[offset:end], end
}
