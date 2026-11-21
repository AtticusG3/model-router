package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// imageReject is a pipeline step refusing a client request. proxyImage answers
// it with 400 {"error": msg} before the backend sees the request.
type imageReject string

func (e imageReject) Error() string { return string(e) }

// checkImg2img validates an img2img body: sd-server drops a missing or
// unreadable init image and quietly runs txt2img instead. It also rewrites a
// data-URL first init to plain base64 and mirrors it into an empty
// extra_images, so vision-conditioned reference images get the hero image.
// Bodies that are not a JSON object, and other paths, pass through untouched.
func checkImg2img(req *http.Request, notes *imageNotes) error {
	if req.Method != http.MethodPost || req.URL.Path != "/sdapi/v1/img2img" || req.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	// RawMessage values keep untouched fields byte-exact.
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil
	}

	var inits []json.RawMessage
	if json.Unmarshal(obj["init_images"], &inits) != nil || len(inits) == 0 {
		return imageReject("img2img requires non-empty init_images")
	}
	var first string
	_ = json.Unmarshal(inits[0], &first)
	img := decodeB64Image(first)
	if img == nil {
		return imageReject("init_images[0] is not a decodable PNG/JPEG/WEBP")
	}

	changed := false
	if strings.HasPrefix(first, "data:") {
		inits[0], _ = json.Marshal(base64.StdEncoding.EncodeToString(img))
		obj["init_images"], _ = json.Marshal(inits)
		notes.Add("img2img: data-URL init_images[0] sent as plain base64")
		changed = true
	}
	var extras []json.RawMessage
	if json.Unmarshal(obj["extra_images"], &extras) != nil || len(extras) == 0 {
		obj["extra_images"], _ = json.Marshal([]json.RawMessage{inits[0]})
		notes.Add("img2img: init_images[0] mirrored into extra_images")
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(out))
	req.ContentLength = int64(len(out))
	return nil
}

// decodeB64Image decodes a base64 image, with or without a data-URL prefix,
// and returns nil unless it starts like a PNG, JPEG or WEBP file.
func decodeB64Image(v string) []byte {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "data:") {
		if _, after, ok := strings.Cut(v, ","); ok {
			v = after
		}
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(raw) < 24 {
		return nil
	}
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")),
		bytes.HasPrefix(raw, []byte("\xff\xd8\xff")),
		string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		return raw
	}
	return nil
}
