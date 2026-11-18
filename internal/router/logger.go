package router

import (
	"fmt"
	"io"
	"regexp"
	"sort"
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
	recent   []logEntry
	upstream []logEntry
	mesh     []logEntry
	// byModel keeps a ring per model so one chatty backend cannot push
	// another model's history out of its per-model stream.
	byModel map[string][]logEntry
	seq     uint64
	subs    map[*logSub]struct{}
}

// Stream kinds a line can belong to.
const (
	streamProxy    = "proxy"
	streamUpstream = "upstream"
	streamMesh     = "mesh"
)

type logEntry struct {
	seq    uint64
	stream string
	model  string // model id without the #n copy suffix; upstream lines only
	text   string
}

type logSub struct {
	match func(logEntry) bool
	ch    chan string
}

const maxRecentLogs = 300

func NewLogger(w io.Writer, verbose bool) *Logger { return &Logger{w: w, verb: verbose} }

func appendRing(dst []logEntry, e logEntry) []logEntry {
	dst = append(dst, e)
	if len(dst) > maxRecentLogs {
		return dst[len(dst)-maxRecentLogs:]
	}
	return dst
}

func copyRing(src []logEntry, limit int) []string {
	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}
	start := len(src) - limit
	out := make([]string, limit)
	for i, e := range src[start:] {
		out[i] = e.text
	}
	return out
}

// ansiSeq matches ANSI/VT100 escape sequences. llama-server and the Python
// backends emit colour codes and cursor moves on stdout; those look like
// garbage in the browser and are useless in a ring buffer, so strip them.
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// maxRingLine caps one stored line. A backend that dumps a multi-megabyte
// stack trace must not bloat the rings or every open log stream.
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

func (l *Logger) write(stream, model, prefix, format string, args ...any) {
	line := prefix + fmt.Sprintf(format, args...)
	// The ring gets a time-of-day prefix so an operator can tell when an
	// event happened. stdout does not: journald already stamps each line and
	// a second timestamp only makes `journalctl -o short` harder to read.
	stamped := time.Now().Format("15:04:05.000") + " " + sanitize(line)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	// Copies (id#n) file under their model so its stream shows them all; the text keeps the #n tag.
	e := logEntry{seq: l.seq, stream: stream, model: instanceModel(model), text: stamped}
	switch stream {
	case streamUpstream:
		l.upstream = appendRing(l.upstream, e)
		if model != "" {
			if l.byModel == nil {
				l.byModel = map[string][]logEntry{}
			}
			l.byModel[e.model] = appendRing(l.byModel[e.model], e)
		}
	case streamMesh:
		l.mesh = appendRing(l.mesh, e)
	default:
		l.recent = appendRing(l.recent, e)
	}
	for sub := range l.subs {
		if !sub.match(e) {
			continue
		}
		select {
		case sub.ch <- stamped:
		default:
			// shortcut: a stalled client drops lines instead of blocking the logger, upgrade if a client needs lossless delivery
		}
	}
	fmt.Fprintln(l.w, line)
}

func (l *Logger) Infof(format string, args ...any) {
	l.write(streamProxy, "", "[router] ", format, args...)
}

func (l *Logger) Debugf(format string, args ...any) {
	if !l.verb {
		return
	}
	l.write(streamProxy, "", "[router:debug] ", format, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.write(streamProxy, "", "[router:warn] ", format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.write(streamProxy, "", "[router:error] ", format, args...)
}

// Upstreamf logs one backend stdout/stderr line, tagged with its model id.
func (l *Logger) Upstreamf(model, format string, args ...any) {
	l.write(streamUpstream, model, "[upstream] ["+model+"] ", format, args...)
}

func (l *Logger) Meshf(format string, args ...any) {
	l.write(streamMesh, "", "[mesh] ", format, args...)
}

// Subscribe returns the buffered history for a stream and a channel of lines
// written after it. Both are taken under one lock so nothing is lost or
// repeated between them. stream is "" (combined), proxy, upstream or mesh;
// model, when set, narrows to that model's upstream lines. Call cancel when done.
func (l *Logger) Subscribe(stream, model string) (history []string, live <-chan string, cancel func()) {
	match := func(e logEntry) bool {
		if model != "" {
			return e.stream == streamUpstream && e.model == model
		}
		return stream == "" || e.stream == stream
	}
	sub := &logSub{match: match, ch: make(chan string, 256)}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case model != "":
		history = copyRing(l.byModel[model], 0)
	case stream == streamProxy:
		history = copyRing(l.recent, 0)
	case stream == streamUpstream:
		history = copyRing(l.upstream, 0)
	case stream == streamMesh:
		history = copyRing(l.mesh, 0)
	default:
		history = mergeRings(l.recent, l.upstream, l.mesh)
	}
	if l.subs == nil {
		l.subs = map[*logSub]struct{}{}
	}
	l.subs[sub] = struct{}{}
	return history, sub.ch, func() {
		l.mu.Lock()
		delete(l.subs, sub)
		l.mu.Unlock()
	}
}

// mergeRings interleaves the rings in write order.
func mergeRings(rings ...[]logEntry) []string {
	var all []logEntry
	for _, r := range rings {
		all = append(all, r...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	return copyRing(all, 0)
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
