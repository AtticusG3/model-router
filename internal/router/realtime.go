package router

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// realtimeFirstFrameWait is how long a realtime upgrade with no model in the
// URL waits for the client's first frame. A var so tests can shorten it.
var realtimeFirstFrameWait = 5 * time.Second

// sessionMinute is one session_max_minutes unit. A var so tests can shorten it.
var sessionMinute = time.Minute

// maxFirstFrame bounds the first client frame the router reads for a model id.
const maxFirstFrame = 1 << 20

// isWebsocketUpgrade reports whether req asks to switch to a websocket.
func isWebsocketUpgrade(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
}

// sessionWriter wraps a proxied websocket upgrade. Once the connection is
// hijacked it counts the session on model_router_realtime_sessions until the
// proxy returns, and closes it with 1001 at session_max_minutes. The proxy call
// it runs under already holds the instance's occupancy slot, so the idle sweep
// and eviction treat the model as busy for the whole session.
type sessionWriter struct {
	http.ResponseWriter
	r     *Router
	model string
	ws    *wsConn
	timer *time.Timer
}

func (s *sessionWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Hijack hands the proxy the client connection wrapped in a wsConn, which
// counts frames for the Activity row and can send the 1001 close. The 101 head
// still goes out through the inner ReadWriter, so it is not read as a frame;
// ReverseProxy copies frames over the returned conn.
func (s *sessionWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(s.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	// Bytes the server read ahead of the hijack come first, then the socket.
	src := io.Reader(conn)
	if n := brw.Reader.Buffered(); n > 0 {
		ahead, _ := brw.Reader.Peek(n)
		src = io.MultiReader(bytes.NewReader(ahead), conn)
	}
	s.ws = &wsConn{Conn: conn, r: src}
	s.r.metrics.AddRealtimeSession(s.model, 1)
	if n := s.r.cfg().SessionMaxMinutes; n > 0 {
		s.timer = time.AfterFunc(time.Duration(n)*sessionMinute, s.ws.goAway)
	}
	return s.ws, bufio.NewReadWriter(bufio.NewReader(s.ws), brw.Writer), nil
}

// end runs when the proxy returns, which for an upgrade is when the session
// closes.
func (s *sessionWriter) end() {
	if s.ws == nil {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.r.metrics.AddRealtimeSession(s.model, -1)
}

// upgradedWriter stands in for the client's writer once the router has
// answered a websocket upgrade itself. The backend's own 101 head is dropped,
// and an HTTP error written instead of a 101 becomes a close frame on finish,
// since a status line cannot follow a 101.
type upgradedWriter struct {
	conn     net.Conn // replays the frames read while picking the model
	h        http.Header
	status   int
	reason   bytes.Buffer
	hijacked bool
	gone     bool // the client left before a model was picked
}

func (u *upgradedWriter) Header() http.Header { return u.h }

func (u *upgradedWriter) WriteHeader(code int) {
	if u.status == 0 {
		u.status = code
	}
}

func (u *upgradedWriter) Write(p []byte) (int, error) {
	u.WriteHeader(http.StatusOK)
	// A close reason holds at most 123 bytes.
	if n := 123 - u.reason.Len(); n > 0 {
		u.reason.Write(p[:min(n, len(p))])
	}
	return len(p), nil
}

func (u *upgradedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	u.hijacked = true
	return u.conn, bufio.NewReadWriter(bufio.NewReader(u.conn), bufio.NewWriter(io.Discard)), nil
}

// finish closes the client connection unless the proxy took it over: with
// 1008 and the error text for a 4xx (no such model), else 1011.
func (u *upgradedWriter) finish() {
	if u.hijacked {
		return // the proxy closes the connection when the session ends
	}
	if !u.gone {
		code := 1011
		if u.status > 0 && u.status < 500 {
			code = 1008
		}
		reason := bytes.TrimSpace(u.reason.Bytes())
		frame := []byte{0x88, byte(2 + len(reason)), byte(code >> 8), byte(code)}
		_, _ = u.conn.Write(append(frame, reason...))
	}
	_ = u.conn.Close()
}

// answerUpgrade serves a websocket upgrade that names no model in the URL. It
// answers the handshake itself, waits realtimeFirstFrameWait for the client's
// first frame and copies its session.model (sent in session.update) into the
// query, so the session then routes like ?model= would, peer rewrite included.
// The frames it read reach the backend unchanged, ahead of the rest. With no
// model in the frame, or no frame in time, the path's api_type default serves
// the session. nil means the connection cannot be taken over (no key, or a
// writer that cannot hijack), so the request carries on as plain HTTP.
func (r *Router) answerUpgrade(w http.ResponseWriter, req *http.Request) *upgradedWriter {
	key := req.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil
	}
	u := &upgradedWriter{conn: conn, h: http.Header{}}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) // RFC 6455 accept GUID
	head := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n"
	// shortcut: echoes the client's first offered subprotocol, not the backend's pick, dial the backend before answering if a client needs another.
	if p, _, _ := strings.Cut(req.Header.Get("Sec-WebSocket-Protocol"), ","); strings.TrimSpace(p) != "" {
		head += "Sec-WebSocket-Protocol: " + strings.TrimSpace(p) + "\r\n"
	}
	// No extension was agreed with the client, so the backend must not agree one.
	req.Header.Del("Sec-WebSocket-Extensions")
	if _, err := brw.WriteString(head + "\r\n"); err != nil || brw.Flush() != nil {
		u.gone = true
		return u
	}
	var read bytes.Buffer
	_ = conn.SetReadDeadline(time.Now().Add(realtimeFirstFrameWait))
	text, err := readFirstText(io.TeeReader(brw.Reader, &read), maxFirstFrame)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		u.gone = true
		return u
	}
	ahead, _ := brw.Reader.Peek(brw.Reader.Buffered())
	u.conn = &wsConn{Conn: conn, r: io.MultiReader(bytes.NewReader(append(read.Bytes(), ahead...)), conn)}
	var ev struct {
		Session struct {
			Model string `json:"model"`
		} `json:"session"`
	}
	if json.Unmarshal(text, &ev) == nil && ev.Session.Model != "" {
		q := req.URL.Query()
		q.Set("model", ev.Session.Model)
		req.URL.RawQuery = q.Encode()
	}
	return u
}

// readFirstText reads client frames up to the first data frame and returns its
// unmasked payload when that is a text frame of at most limit bytes; nil for a
// binary, continuation or larger frame, whose payload is left unread. Control
// frames before it are read past, and a close frame is io.EOF.
func readFirstText(r io.Reader, limit uint64) ([]byte, error) {
	for {
		var h [14]byte
		if _, err := io.ReadFull(r, h[:2]); err != nil {
			return nil, err
		}
		op, n, ext, maskLen := h[0]&0x0f, uint64(h[1]&0x7f), 0, 0
		switch n {
		case 126:
			ext = 2
		case 127:
			ext = 8
		}
		if h[1]&0x80 != 0 {
			maskLen = 4
		}
		rest := h[2 : 2+ext+maskLen]
		if _, err := io.ReadFull(r, rest); err != nil {
			return nil, err
		}
		switch ext {
		case 2:
			n = uint64(binary.BigEndian.Uint16(rest))
		case 8:
			n = binary.BigEndian.Uint64(rest)
		}
		if n > limit || (op < 8 && op != 1) {
			return nil, nil
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
		switch op {
		case 1:
			if maskLen == 4 {
				for i := range payload {
					payload[i] ^= rest[ext+i%4]
				}
			}
			return payload, nil
		case 8:
			return nil, io.EOF
		}
	}
}
