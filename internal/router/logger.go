package router

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Logger is a minimal leveled logger to keep the router dependency-free.
// Recent entries are exposed to the local WebUI; the process stdout/stderr
// remains the durable source for systemd/journald.
type Logger struct {
	mu       sync.Mutex
	w        io.Writer
	verb     bool
	recent   []string
	upstream []string
	mesh     []string
}

const maxRecentLogs = 300

func NewLogger(w io.Writer, verbose bool) *Logger { return &Logger{w: w, verb: verbose} }

func appendRing(dst []string, line string) []string {
	dst = append(dst, line)
	if len(dst) > maxRecentLogs {
		return dst[len(dst)-maxRecentLogs:]
	}
	return dst
}

func copyRing(src []string, limit int) []string {
	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}
	start := len(src) - limit
	out := make([]string, limit)
	copy(out, src[start:])
	return out
}

// ansiSeq matches ANSI/VT100 escape sequences. llama-server and the Python
// backends emit colour codes and cursor moves on stdout; those look like
// garbage in the browser and are useless in a ring buffer, so strip them.
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// maxRingLine caps one stored line. A backend that dumps a multi-megabyte
// stack trace must not bloat every 3-second poll of /_router/logs.
const maxRingLine = 2000

// sanitize makes one log line safe to store and render: no escape sequences,
// no carriage returns from progress bars, and bounded length. The stdout copy
// is left untouched so journald keeps the raw bytes.
func sanitize(s string) string {
	if strings.ContainsRune(s, '\x1b') {
		s = ansiSeq.ReplaceAllString(s, "")
	}
	if strings.ContainsRune(s, '\r') {
		s = strings.ReplaceAll(s, "\r", "")
	}
	if len(s) > maxRingLine {
		cut := maxRingLine
		// Do not split a multi-byte rune in half.
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func (l *Logger) write(ring *[]string, prefix, format string, args ...any) {
	line := prefix + fmt.Sprintf(format, args...)
	// The ring gets a time-of-day prefix so an operator can tell when an
	// event happened. stdout does not: journald already stamps each line and
	// a second timestamp only makes `journalctl -o short` harder to read.
	stamped := time.Now().Format("15:04:05.000") + " " + sanitize(line)
	l.mu.Lock()
	defer l.mu.Unlock()
	*ring = appendRing(*ring, stamped)
	fmt.Fprintln(l.w, line)
}

func (l *Logger) Infof(format string, args ...any) {
	l.write(&l.recent, "[router] ", format, args...)
}

func (l *Logger) Debugf(format string, args ...any) {
	if !l.verb {
		return
	}
	l.write(&l.recent, "[router:debug] ", format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.write(&l.recent, "[router:error] ", format, args...)
}

func (l *Logger) Upstreamf(format string, args ...any) {
	l.write(&l.upstream, "[upstream] ", format, args...)
}

func (l *Logger) Meshf(format string, args ...any) {
	l.write(&l.mesh, "[mesh] ", format, args...)
}

// RecentLogs returns the newest router log entries, newest last.
func (l *Logger) RecentLogs(limit int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return copyRing(l.recent, limit)
}

func (l *Logger) RecentUpstream(limit int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return copyRing(l.upstream, limit)
}

func (l *Logger) RecentMesh(limit int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return copyRing(l.mesh, limit)
}
