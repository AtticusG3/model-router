package router

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImagePipelineImg2imgChecks(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)))
	jpeg := base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff" + strings.Repeat("\x00", 21)))
	webp := base64.StdEncoding.EncodeToString([]byte("RIFF\x00\x00\x00\x00WEBP" + strings.Repeat("\x00", 12)))
	text := base64.StdEncoding.EncodeToString([]byte("plain text, not an image at all"))

	const img2img, txt2img = "/sdapi/v1/img2img", "/sdapi/v1/txt2img"
	cases := []struct {
		name, path, body string
		wantErr          string // non-empty: 400 with this message, backend not hit
		wantBody         string // backend body; "" means the client's body unchanged
	}{
		{name: "missing inits", path: img2img, body: `{"prompt":"cat"}`, wantErr: "img2img requires non-empty init_images"},
		{name: "empty inits", path: img2img, body: `{"init_images":[]}`, wantErr: "img2img requires non-empty init_images"},
		{name: "null inits", path: img2img, body: `{"init_images":null}`, wantErr: "img2img requires non-empty init_images"},
		{name: "undecodable init", path: img2img, body: `{"init_images":["` + text + `"]}`, wantErr: "init_images[0] is not a decodable PNG/JPEG/WEBP"},
		{name: "non-string init", path: img2img, body: `{"init_images":[7]}`, wantErr: "init_images[0] is not a decodable PNG/JPEG/WEBP"},
		{name: "data-URL init", path: img2img,
			body:     `{"init_images":["data:image/png;base64,` + png + `","` + jpeg + `"],"prompt":"cat"}`,
			wantBody: `{"extra_images":["` + png + `"],"init_images":["` + png + `","` + jpeg + `"],"prompt":"cat"}`},
		{name: "extras mirrored", path: img2img, body: `{"init_images":["` + jpeg + `"],"extra_images":[]}`,
			wantBody: `{"extra_images":["` + jpeg + `"],"init_images":["` + jpeg + `"]}`},
		{name: "extras kept", path: img2img, body: `{"init_images":["` + webp + `"],"extra_images":["` + png + `"],"cfg_scale":7.50}`},
		{name: "non-JSON passthrough", path: img2img, body: `not json`},
		{name: "JSON array passthrough", path: img2img, body: `[1,2]`},
		{name: "txt2img untouched", path: txt2img, body: `{"prompt":"cat","init_images":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			var got []byte
			reply := func(w http.ResponseWriter, req *http.Request) {
				hits++
				got, _ = io.ReadAll(req.Body)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"images":["ok"]}`)
			}
			mux := imageBackend(t, reply).(*http.ServeMux)
			mux.HandleFunc(img2img, reply)
			r := imageTestRouter(t, mux)

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			NewHandler(r, NewLogger(io.Discard, false)).ServeHTTP(rec, req)

			if tc.wantErr != "" {
				var e struct{ Error string }
				_ = json.Unmarshal(rec.Body.Bytes(), &e)
				if rec.Code != http.StatusBadRequest || e.Error != tc.wantErr {
					t.Fatalf("reply = %d %s, want 400 %q", rec.Code, rec.Body.String(), tc.wantErr)
				}
				if h := rec.Header().Get("X-SD-Shim"); h != "reject" {
					t.Fatalf("X-SD-Shim = %q, want reject", h)
				}
				if hits != 0 {
					t.Fatalf("backend hit %d times by a rejected request", hits)
				}
				return
			}
			if rec.Code != http.StatusOK || hits != 1 {
				t.Fatalf("reply = %d %s after %d backend hits", rec.Code, rec.Body.String(), hits)
			}
			want := tc.wantBody
			if want == "" {
				want = tc.body
			}
			if string(got) != want {
				t.Fatalf("backend body = %s\nwant %s", got, want)
			}
		})
	}
}
