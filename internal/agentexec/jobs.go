package agentexec

import (
	"bytes"
	"strings"
	"sync"
	"time"
)

// JobState is where one agent command stands.
type JobState string

const (
	JobRunning     JobState = "running"
	JobDone        JobState = "done"        // the envelope's marker arrived with the exit status
	JobInterrupted JobState = "interrupted" // Ctrl-C was sent; whether it stopped is not known
	JobEnded       JobState = "ended"       // the user pressed Done, or a confirm-only job reached its limit
	JobUnknown     JobState = "unknown"     // the terminal ended or the connection dropped: the outcome is unknown
)

// Limits on a job (KTD34).
const (
	maxJobOutput = 1 << 20
	readChunk    = 64 << 10
	confirmLimit = 5 * time.Minute // a job with no envelope ends here unless Done comes first
	jobRetention = 10 * time.Minute
)

// Job is one command the agent ran in a terminal.
type Job struct {
	ID, Principal, Target, Command string
	Shell                          Shell
	nonce                          string
	started                        time.Time

	mu        sync.Mutex
	state     JobState
	exit      int
	out       []byte
	truncated bool
	ended     time.Time
	done      chan struct{}
}

// JobView is a job's state and a slice of its output.
type JobView struct {
	ID        string   `json:"id"`
	State     JobState `json:"state"`
	Exit      *int     `json:"exit,omitempty"`
	Output    string   `json:"output"`
	Offset    int      `json:"offset"` // where Output starts in the job's output
	Next      int      `json:"next"`   // the offset to read from next
	Truncated bool     `json:"truncated,omitempty"`
}

func (j *Job) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state == JobRunning
}

func (j *Job) append(p []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if room := maxJobOutput - len(j.out); room < len(p) {
		p, j.truncated = p[:max(0, room)], true
	}
	j.out = append(j.out, p...)
}

// finish ends a running job once; it reports whether this call ended it.
func (j *Job) finish(state JobState, exit int, now time.Time) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != JobRunning {
		return false
	}
	j.state, j.exit, j.ended = state, exit, now
	if state == JobDone {
		if i := bytes.LastIndex(j.out, marker(j.nonce)); i >= 0 {
			j.out = j.out[:i]
		}
	}
	close(j.done)
	return true
}

// view scrubs the whole output, then returns up to readChunk of it from
// offset; offset < 0 reads the last readChunk. Offsets count in the
// scrubbed output. While the job runs, the last bytes a secret could start
// in are held back, as HostJob.view does. The nonce never reaches the model.
func (j *Job) view(offset int, secrets []string) JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	hold := 0
	if j.state == JobRunning {
		for _, s := range secrets {
			hold = max(hold, len(s)-1)
		}
	}
	s := Scrub(strings.ReplaceAll(string(j.out), j.nonce, ""), secrets)
	if offset < 0 {
		offset = max(0, len(s)-hold-readChunk)
	}
	offset = min(offset, max(0, len(s)-hold))
	v := JobView{ID: j.ID, State: j.state, Offset: offset, Truncated: j.truncated}
	v.Output, v.Next = slice(s, offset, hold)
	if j.state == JobDone {
		v.Exit = new(j.exit)
	}
	return v
}
