package monitor

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Bounds from KTD29.
const (
	DefaultInterval = 3 * time.Second
	MinInterval     = 2 * time.Second
	MaxInterval     = 30 * time.Second
	SampleTimeout   = 10 * time.Second
	MaxOutput       = 1 << 20
	MaxBackoff      = 30 * time.Second
	HistoryLen      = 300
)

// Sampler states, as Event.State.
const (
	StateStarting     = "starting"
	StateLive         = "live"
	StateError        = "error"        // the last sample failed; backing off
	StateDisconnected = "disconnected" // the connection dropped; waiting for the session's reconnect
	StateUnsupported  = "unsupported"  // not Linux; sampling stopped
)

// Event is what a sampler reports after each tick: its state and, when a
// sample came in, the sample.
type Event struct {
	State  string  `json:"state"`
	Error  string  `json:"error,omitempty"`
	OS     string  `json:"os,omitempty"` // uname -s, for the unsupported state
	Sample *Sample `json:"sample,omitempty"`
}

// Config is what a Sampler runs on. Client returns the connection to sample
// over, or nil while it is dropped. Tick replaces the interval's ticker in
// tests.
type Config struct {
	Client   func() *ssh.Client
	Emit     func(Event)
	Interval time.Duration
	Tick     <-chan time.Time
}

// Sampler runs Script on a host every interval with one sample in flight at
// a time: a tick that finds one still running is skipped, not queued.
// Failures back off, doubling up to MaxBackoff. It keeps the last
// HistoryLen samples for new subscribers.
type Sampler struct {
	cfg    Config
	ticker *time.Ticker // nil when cfg.Tick is set
	kick   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	interval time.Duration
	paused   bool
	last     Event
	ring     []Sample // without processes or ports, which only the newest needs
	newest   *Sample
}

// New starts sampling at once.
func New(cfg Config) *Sampler {
	s := &Sampler{cfg: cfg, kick: make(chan struct{}, 1), interval: clampInterval(cfg.Interval), last: Event{State: StateStarting}}
	tick := cfg.Tick
	if tick == nil {
		s.ticker = time.NewTicker(s.interval)
		tick = s.ticker.C
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.kick <- struct{}{}
	s.wg.Add(1)
	go s.loop(ctx, tick)
	return s
}

func clampInterval(d time.Duration) time.Duration {
	if d == 0 {
		return DefaultInterval
	}
	return min(max(d, MinInterval), MaxInterval)
}

// Stop ends sampling and returns once no exec is running or can start.
func (s *Sampler) Stop() {
	s.cancel()
	s.wg.Wait()
	if s.ticker != nil {
		s.ticker.Stop()
	}
}

// SetPaused pauses sampling while no subscriber can see it. Resuming
// samples at once.
func (s *Sampler) SetPaused(p bool) {
	s.mu.Lock()
	was := s.paused
	s.paused = p
	s.mu.Unlock()
	if was && !p {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// SetInterval changes the interval, clamped to MinInterval..MaxInterval.
func (s *Sampler) SetInterval(d time.Duration) time.Duration {
	d = clampInterval(d)
	s.mu.Lock()
	s.interval = d
	s.mu.Unlock()
	if s.ticker != nil {
		s.ticker.Reset(d)
	}
	return d
}

func (s *Sampler) Interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval
}

// Last is the most recent event, without its sample.
func (s *Sampler) Last() Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.last
	e.Sample = nil
	return e
}

// History is the kept samples, oldest first; only the newest carries
// processes and ports.
func (s *Sampler) History() []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Sample{}, s.ring...)
	if s.newest != nil {
		out[len(out)-1] = *s.newest
	}
	return out
}

type result struct {
	at      time.Time
	out     string
	partial bool
	err     error
}

func (s *Sampler) loop(ctx context.Context, tick <-chan time.Time) {
	defer s.wg.Done()
	var (
		done      = make(chan result, 1)
		inFlight  bool
		fails     int
		notBefore time.Time
		prev      *counters
		prevAt    time.Time
	)
	start := func() {
		s.mu.Lock()
		paused := s.paused
		s.mu.Unlock()
		if inFlight || paused || time.Now().Before(notBefore) {
			return
		}
		c := s.cfg.Client()
		if c == nil {
			prev = nil
			s.report(Event{State: StateDisconnected}, false)
			return
		}
		inFlight = true
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(ctx, SampleTimeout)
			defer cancel()
			at := time.Now()
			out, partial, err := run(ctx, c, MaxOutput)
			done <- result{at, out, partial, err}
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
			start()
		case <-tick:
			start()
		case r := <-done:
			inFlight = false
			if ctx.Err() != nil {
				return
			}
			p := parse(r.out)
			if r.err == nil && p.os == "" {
				r.err = errors.New("the host returned no figures")
			}
			if r.err != nil {
				fails = min(fails+1, 8) // bounds the shift; MaxBackoff caps the wait anyway
				notBefore = time.Now().Add(min(s.Interval()<<fails, MaxBackoff))
				s.report(Event{State: StateError, Error: r.err.Error()}, false)
				continue
			}
			fails, notBefore = 0, time.Time{}
			if p.os != "Linux" {
				s.report(Event{State: StateUnsupported, OS: p.os}, false)
				return
			}
			sm := finish(prev, p, r.at.Sub(prevAt).Seconds())
			sm.Time, sm.Partial = r.at.UnixMilli(), r.partial
			prev, prevAt = &p.c, r.at
			s.report(Event{State: StateLive, Sample: &sm}, true)
		}
	}
}

// report records e, keeps its sample and emits it.
func (s *Sampler) report(e Event, keep bool) {
	s.mu.Lock()
	same := e.Sample == nil && s.last.State == e.State && s.last.Error == e.Error
	s.last = e
	if keep {
		lite := *e.Sample
		lite.Procs, lite.Ports = nil, nil
		if len(s.ring) == HistoryLen {
			s.ring = s.ring[1:]
		}
		s.ring = append(s.ring, lite)
		s.newest = e.Sample
	}
	s.mu.Unlock()
	if !same { // a state repeated without a sample says nothing new
		s.cfg.Emit(e)
	}
}

// run runs Script on its own channel and returns at most limit bytes of its
// output; partial is set when there was more, and the rest is not read.
func run(ctx context.Context, c *ssh.Client, limit int) (string, bool, error) {
	sess, err := c.NewSession()
	if err != nil {
		return "", false, err
	}
	defer sess.Close()
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return "", false, err
	}
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	if err := sess.Start(Script); err != nil {
		return "", false, err
	}
	b, err := io.ReadAll(io.LimitReader(stdout, int64(limit)+1))
	if ctx.Err() != nil {
		return "", false, ctx.Err()
	}
	if len(b) > limit {
		return string(b[:limit]), true, nil
	}
	if err != nil {
		return "", false, err
	}
	// The script's exit status means nothing: each part falls back on its
	// own. Only a channel that broke is a failure.
	var exit *ssh.ExitError
	var missing *ssh.ExitMissingError
	if err := sess.Wait(); err != nil && !errors.As(err, &exit) && !errors.As(err, &missing) {
		return "", false, err
	}
	return string(b), false, nil
}
