package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// maxClipSource is how much of a JSON request or response body is buffered
	// so it can be clipped field by field down to maxCaptureBody. A longer body
	// is cut as raw bytes and flagged truncated.
	maxClipSource = 8 << 20
	// clipFloor is the shortest string literal ever clipped; shorter strings
	// are kept whole.
	clipFloor = 256
	// markerMax bounds a clip marker: the quoted "[image 0: png 1.3 MB]"
	// literal or the SSE comment line that counts dropped events.
	markerMax = 64
)

// clipCapture fits a body into maxCaptureBody for the Activity capture. A JSON
// body, or an SSE stream of JSON events, is clipped inside so it still parses;
// fields names what was shortened, each prefixed with prefix. Anything else is
// cut as raw bytes and truncated is set. A short body is returned unchanged.
func clipCapture(b []byte, prefix string) (body string, fields []string, truncated bool) {
	if len(b) <= maxCaptureBody {
		return string(b), nil, false
	}
	if out, f, ok := clipJSON(b, maxCaptureBody, prefix); ok {
		return string(out), f, false
	}
	if out, f, ok := clipSSE(b, maxCaptureBody, prefix); ok {
		return string(out), f, false
	}
	return string(b[:maxCaptureBody]), nil, true
}

// captureLimit is how much of a request body to keep: a JSON body up to limit
// so it can be clipped field by field, anything else (an upload) maxCaptureBody.
func captureLimit(b []byte, limit int) int {
	if t := bytes.TrimLeft(b, " \t\r\n"); len(t) > 0 && (t[0] == '{' || t[0] == '[') {
		return limit
	}
	return min(limit, maxCaptureBody)
}

// jsonString is one string value in a JSON body: its quoted literal spans
// raw[start:end].
type jsonString struct {
	start, end int
	path       string
	val        string
	img        string // image format when val is a base64 image, else ""
	imgIdx     int
}

// clipJSON shortens the longest string values in a JSON document until it
// fits budget bytes, keeping every other byte as it was. Base64 images become
// a placeholder naming the image. ok is false when b is not one JSON value or
// clipping strings alone cannot make it fit.
func clipJSON(b []byte, budget int, prefix string) ([]byte, []string, bool) {
	strs, ok := longJSONStrings(b)
	if !ok {
		return nil, nil, false
	}
	excess := len(b) - budget
	savings := func(level int) int {
		n := 0
		for _, s := range strs {
			if l := s.end - s.start; l > level {
				if s.img != "" {
					n += l - markerMax
				} else {
					n += l - level
				}
			}
		}
		return n
	}
	if savings(clipFloor) < excess {
		return nil, nil, false
	}
	// The highest level that still saves enough: every string longer than it
	// is cut to it, so the longest strings lose the most.
	lo, hi := clipFloor, len(b)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if savings(mid) >= excess {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	var out bytes.Buffer
	var fields []string
	at := 0
	for _, s := range strs {
		if s.end-s.start <= lo {
			continue
		}
		out.Write(b[at:s.start])
		if s.img != "" {
			lit, _ := json.Marshal(fmt.Sprintf("[image %d: %s %s]", s.imgIdx, s.img, fmtSize(len(s.val)*3/4)))
			out.Write(lit)
		} else {
			out.Write(clipString(s.val, lo))
		}
		at = s.end
		fields = append(fields, prefix+s.path)
	}
	out.Write(b[at:])
	return out.Bytes(), fields, true
}

// longJSONStrings walks b and returns, in document order, the string values
// whose quoted literal is longer than clipFloor. ok is false when b is not
// exactly one JSON value.
func longJSONStrings(b []byte) ([]jsonString, bool) {
	type frame struct {
		obj     bool
		key     string
		idx     int
		wantKey bool
	}
	var stack []frame
	var strs []jsonString
	images := 0
	path := func() string {
		var p strings.Builder
		for i, f := range stack {
			if f.obj {
				if i > 0 {
					p.WriteByte('.')
				}
				p.WriteString(f.key)
			} else {
				p.WriteString("[" + strconv.Itoa(f.idx) + "]")
			}
		}
		return p.String()
	}
	beginValue := func() {
		if n := len(stack); n > 0 && !stack[n-1].obj {
			stack[n-1].idx++
		}
	}
	endValue := func() {
		if n := len(stack); n > 0 && stack[n-1].obj {
			stack[n-1].wantKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	values := 0
	for {
		before := int(dec.InputOffset())
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		if len(stack) == 0 {
			if values++; values > 1 {
				return nil, false
			}
		}
		switch t := tok.(type) {
		case json.Delim:
			if t == '{' || t == '[' {
				beginValue()
				stack = append(stack, frame{obj: t == '{', idx: -1, wantKey: true})
				continue
			}
			stack = stack[:len(stack)-1]
			endValue()
			continue
		case string:
			if n := len(stack); n > 0 && stack[n-1].obj && stack[n-1].wantKey {
				stack[n-1].key, stack[n-1].wantKey = t, false
				continue
			}
			beginValue()
			end := int(dec.InputOffset())
			// Only whitespace, commas and colons sit between tokens, so the
			// literal starts at the first quote after the previous token.
			start := before + bytes.IndexByte(b[before:end], '"')
			if end-start > clipFloor {
				s := jsonString{start: start, end: end, path: path(), val: t}
				if s.img = imageKind(t); s.img != "" {
					s.imgIdx = images
					images++
				}
				strs = append(strs, s)
			}
		default:
			beginValue()
		}
		endValue()
	}
	return strs, values == 1
}

// clipString is s quoted as JSON, its tail replaced by a "…[clipped 1.4 MB]"
// marker so the literal is at most limit bytes (never below clipFloor).
func clipString(s string, limit int) []byte {
	k := min(len(s), limit)
	for {
		for k > 0 && k < len(s) && !utf8.RuneStart(s[k]) {
			k--
		}
		out, _ := json.Marshal(s[:k] + "…[clipped " + fmtSize(len(s)-k) + "]")
		if len(out) <= limit || k == 0 {
			return out
		}
		k = max(0, k-(len(out)-limit))
	}
}

// imageKind is the format of a base64 image string ("png", "jpeg"), or ""
// when s is not one. A data URL names its type; bare base64 is sniffed.
func imageKind(s string) string {
	if rest, ok := strings.CutPrefix(s, "data:image/"); ok {
		if i := strings.Index(rest, ";base64,"); i > 0 {
			return rest[:i]
		}
		return ""
	}
	head, err := base64.StdEncoding.DecodeString(s[:min(len(s), 32)/4*4])
	if err != nil {
		return ""
	}
	kind, ok := strings.CutPrefix(http.DetectContentType(head), "image/")
	if !ok {
		return ""
	}
	return kind
}

// clipSSE fits an SSE stream into budget bytes and keeps it valid SSE: a long
// event has its JSON data clipped, then, if the stream is still too long, the
// first and last events are kept and the middle becomes one comment line
// counting what was dropped.
func clipSSE(b []byte, budget int, prefix string) ([]byte, []string, bool) {
	if !bytes.HasPrefix(b, []byte("data:")) && !bytes.HasPrefix(b, []byte("event:")) && !bytes.HasPrefix(b, []byte(":")) {
		return nil, nil, false
	}
	events := bytes.SplitAfter(b, []byte("\n\n"))
	var fields []string
	perEvent := budget / 8
	total := 0
	for i, ev := range events {
		if len(ev) > perEvent {
			lines := bytes.Split(ev, []byte("\n"))
			for j, l := range lines {
				data, ok := bytes.CutPrefix(l, []byte("data:"))
				if !ok {
					continue
				}
				// A data line that is not clippable JSON stays as it is.
				if out, f, ok := clipJSON(bytes.TrimSpace(data), perEvent, fmt.Sprintf("%sevents[%d].", prefix, i)); ok {
					lines[j] = append([]byte("data: "), out...)
					fields = append(fields, f...)
				}
			}
			events[i] = bytes.Join(lines, []byte("\n"))
		}
		total += len(events[i])
	}
	if total <= budget {
		return bytes.Join(events, nil), fields, true
	}
	// Half the budget for the opening events, the rest less the comment line
	// for the closing ones, which carry the finish reason and usage.
	head, size := 0, 0
	for head < len(events) && size+len(events[head]) <= budget/2 {
		size += len(events[head])
		head++
	}
	tail, size := len(events), 0
	for tail > head && size+len(events[tail-1]) <= budget/2-markerMax {
		size += len(events[tail-1])
		tail--
	}
	dropped := 0
	for _, ev := range events[head:tail] {
		dropped += len(ev)
	}
	out := slices.Concat(events[:head]...)
	out = fmt.Appendf(out, ": [clipped %d events, %s]\n\n", tail-head, fmtSize(dropped))
	out = append(out, slices.Concat(events[tail:]...)...)
	return out, append(fields, fmt.Sprintf("%sevents[%d:%d]", prefix, head, tail)), true
}

// fmtSize is a byte count for a clip marker: "1.4 MB", "312 KB" or "40 B".
func fmtSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
