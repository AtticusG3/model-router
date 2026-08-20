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
	mu     sync.Mutex
	w      io.Writer
	verb   bool
	recent []string
}

const maxRecentLogs = 300

func NewLogger(w io.Writer, verbose bool) *Logger { return &Logger{w: w, verb: verbose} }

func (l *Logger) write(prefix, format string, args ...any) {
	line := prefix + fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recent = append(l.recent, line)
	if len(l.recent) > maxRecentLogs {
		l.recent = l.recent[len(l.recent)-maxRecentLogs:]
	}
	fmt.Fprintln(l.w, line)
}

func (l *Logger) Infof(format string, args ...any) {
	l.write("[router] ", format, args...)
}

func (l *Logger) Debugf(format string, args ...any) {
	if !l.verb {
		return
	}
	l.write("[router:debug] ", format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.write("[router:error] ", format, args...)
}

// RecentLogs returns the newest log entries, newest last.
func (l *Logger) RecentLogs(limit int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.recent) {
		limit = len(l.recent)
	}
	start := len(l.recent) - limit
	out := make([]string, limit)
	copy(out, l.recent[start:])
	return out
}
