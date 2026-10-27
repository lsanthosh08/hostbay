package terminal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Session logs (KTD39). A log holds the tap's output only, never
// keystrokes, with ANSI sequences stripped and each line as the screen
// showed it once it ended, following the shape of uniterm's
// backend/session/output_log.go line processor.

// lines resolves terminal output into lines: escape sequences are dropped,
// a carriage return or backspace moves along the line, and later text
// overwrites what it lands on.
type lines struct {
	line  []rune
	col   int
	pend  []byte // the start of a character whose last bytes have not arrived
	state int
	csi   []byte
}

const (
	sNormal = iota
	sEsc    // after ESC
	sCSI    // ESC [ ... final
	sString // OSC, DCS, APC, PM or SOS, until BEL or ESC \
	sStrEsc // ESC inside a string
	sCharset
)

// maxLine ends a line that never ends, such as binary output, so memory
// stays bounded.
const maxLine = 64 << 10

func (l *lines) feed(p []byte, emit func(string)) {
	data := p
	if len(l.pend) > 0 {
		data = append(l.pend, p...)
		l.pend = nil
	}
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 && !utf8.FullRune(data[i:]) {
			l.pend = append([]byte(nil), data[i:]...)
			return
		}
		i += size
		l.rune(r, emit)
	}
}

func (l *lines) rune(r rune, emit func(string)) {
	switch l.state {
	case sEsc:
		switch r {
		case '[':
			l.state, l.csi = sCSI, l.csi[:0]
		case ']', 'P', '_', '^', 'X':
			l.state = sString
		case '(', ')', '*', '+', '#', '%':
			l.state = sCharset
		default:
			l.state = sNormal
		}
		return
	case sCSI:
		if r >= 0x40 && r <= 0x7e {
			l.state = sNormal
			l.doCSI(r)
		} else if len(l.csi) < 32 {
			l.csi = append(l.csi, byte(r))
		}
		return
	case sString:
		switch r {
		case 0x07:
			l.state = sNormal
		case 0x1b:
			l.state = sStrEsc
		}
		return
	case sStrEsc, sCharset:
		l.state = sNormal
		return
	}
	switch {
	case r == 0x1b:
		l.state = sEsc
	case r == '\n':
		emit(string(l.line))
		l.line, l.col = l.line[:0], 0
	case r == '\r':
		l.col = 0
	case r == '\b':
		l.col = max(0, l.col-1)
	case r == '\t':
		l.put(r)
	case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
	default:
		l.put(r)
		if len(l.line) >= maxLine {
			l.flush(emit)
		}
	}
}

func (l *lines) put(r rune) {
	for len(l.line) < l.col {
		l.line = append(l.line, ' ')
	}
	if l.col < len(l.line) {
		l.line[l.col] = r
	} else {
		l.line = append(l.line, r)
	}
	l.col++
}

// doCSI applies the sequences that change the line's text: erase in line
// and cursor forward or back. The rest only style or move elsewhere.
func (l *lines) doCSI(final rune) {
	switch final {
	case 'K':
		switch string(l.csi) {
		case "", "0":
			l.line = l.line[:min(l.col, len(l.line))]
		case "1":
			for i := 0; i < min(l.col+1, len(l.line)); i++ {
				l.line[i] = ' '
			}
		case "2":
			l.line = l.line[:0]
		}
	case 'C', 'D':
		n := 1
		fmt.Sscanf(string(l.csi), "%d", &n)
		if final == 'C' {
			// The count comes from the remote; past maxLine put would pad
			// the line with as many spaces as it asks for.
			l.col = min(l.col+min(max(n, 1), maxLine), maxLine)
		} else {
			l.col = max(0, l.col-max(n, 1))
		}
	}
}

// flush emits the unfinished line, if it holds anything, and starts a new
// one.
func (l *lines) flush(emit func(string)) {
	if len(l.line) > 0 {
		emit(string(l.line))
	}
	l.line, l.col = l.line[:0], 0
}

// current is the unterminated line as the screen shows it.
func (l *lines) current() string { return string(l.line) }

// gapLine marks output the tap dropped because the log fell behind.
const gapLine = "[Hostbay: some output was not logged here]"

// secretMarker replaces a known secret the remote printed.
const secretMarker = "[secret]"

// Logger writes one terminal's output to a log file.
type Logger struct {
	sub  *Subscription
	done chan struct{}
}

// StartLog writes what sub receives to w, line by line, with every one of
// secrets replaced by a marker, until Stop or the terminal ends. A line
// marks where the tap dropped output. A write error stops the log and is
// passed to onErr; the terminal is not affected.
func StartLog(sub *Subscription, w io.WriteCloser, secrets []string, onErr func(error)) *Logger {
	l := &Logger{sub: sub, done: make(chan struct{})}
	go l.run(w, secrets, onErr)
	return l
}

func (l *Logger) run(w io.WriteCloser, secrets []string, onErr func(error)) {
	defer close(l.done)
	var pairs []string
	for _, s := range secrets {
		if s != "" {
			pairs = append(pairs, s, secretMarker)
		}
	}
	scrub := strings.NewReplacer(pairs...)
	bw := bufio.NewWriter(w)
	var lp lines
	var err error
	write := func(s string) {
		if err == nil {
			_, err = bw.WriteString(scrub.Replace(s) + "\n")
		}
	}
	var gaps uint64
	for chunk := range l.sub.C {
		if g := l.sub.Gaps(); g != gaps {
			gaps = g
			lp.flush(write)
			write(gapLine)
		}
		lp.feed(chunk, write)
		if err == nil {
			err = bw.Flush()
		}
		if err != nil {
			l.sub.Close()
			w.Close()
			if onErr != nil {
				onErr(err)
			}
			return
		}
	}
	lp.flush(write)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil && onErr != nil {
		onErr(err)
	}
}

// Stop ends the log, writing the unfinished line, and closes the file.
func (l *Logger) Stop() {
	l.sub.Close()
	<-l.done
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// OpenLogFile creates a new log file in dir named by alias and time, mode
// 0600, creating dir 0700 when missing. It never opens an existing file,
// so never one through a link planted at its name: a taken name gets a
// numbered suffix. A dir that is itself a link is refused.
func OpenLogFile(dir, alias string, at time.Time) (*os.File, error) {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir():
		return nil, fmt.Errorf("%s is not a folder Hostbay may log into", dir)
	}
	name := unsafeName.ReplaceAllString(alias, "-")
	if name == "" {
		name = "terminal"
	}
	base := filepath.Join(dir, name+"-"+at.Format("20060102-150405"))
	for n := 1; n < 1000; n++ {
		p := base + ".log"
		if n > 1 {
			p = fmt.Sprintf("%s-%d.log", base, n)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
	return nil, errors.New("too many logs with the same name")
}
