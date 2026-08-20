package router

import (
	"fmt"
	"io"
	"sync"
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

func (l *Logger) write(ring *[]string, prefix, format string, args ...any) {
	line := prefix + fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	*ring = appendRing(*ring, line)
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
