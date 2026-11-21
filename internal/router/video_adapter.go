package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"model-router/internal/config"
)

// The sdcpp adapter turns the router's /v1/videos job API into sd-server's
// native /sdcpp/v1/vid_gen + /sdcpp/v1/jobs/{id} shape. It is selected by a
// stanza's job.adapter and holds no state of its own: every poll, content
// fetch and cancel goes straight to the backend.

const (
	sdcppCreatePath = "/sdcpp/v1/vid_gen"
	sdcppJobsPath   = "/sdcpp/v1/jobs/"
	// maxVideoJobBody bounds the JSON submit read for one video job.
	maxVideoJobBody = 1 << 20
	// maxVideoJobContent bounds a job record; a finished clip is base64 inside it.
	maxVideoJobContent = 512 << 20
)

// videoJobAdapter returns the adapter a local target's stanza names, or ""
// when the backend speaks /v1/videos itself.
func (r *Router) videoJobAdapter(t Target) string {
	if t.Local == "" {
		return ""
	}
	s := r.cfg().Stanza(instanceModel(t.Local))
	if s == nil || s.APIType != config.APITypeVideo || s.Job == nil {
		return ""
	}
	if s.Job.Adapter == config.JobAdapterSDCpp {
		return config.JobAdapterSDCpp
	}
	return ""
}

// sdcppJobRecord is the part of sd-server's job record a status follow-up
// reads. It leaves result out: a finished clip is base64 inside the record,
// and a status poll must not pull it into memory.
type sdcppJobRecord struct {
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	Progress json.RawMessage `json:"progress"`
	Created  json.RawMessage `json:"created"`
	Error    json.RawMessage `json:"error"`
}

// sdcppJobContent is the record with the finished clip attached, read only for
// /content.
type sdcppJobContent struct {
	sdcppJobRecord
	Result *struct {
		MimeType string `json:"mime_type"`
		B64JSON  string `json:"b64_json"`
	} `json:"result"`
}

// videoStatus maps a backend state onto the OpenAI job states.
func videoStatus(s string) string {
	switch strings.ToLower(s) {
	case "", "queued", "pending": // a record with no state has just been accepted
		return "queued"
	case "generating", "in_progress", "running", "processing":
		return "in_progress"
	case "completed", "succeeded", "success":
		return "completed"
	case "failed", "error":
		return "failed"
	case "cancelled", "canceled":
		return "failed"
	default:
		// A state the adapter does not know is not progress: end the job rather
		// than hold the model until the TTL.
		return "failed"
	}
}

// videoProgress reads sd-server's current-pass progress (an object with
// completed/total, or a bare number) as a 0-100 percentage.
func videoProgress(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return clampProgress(int(n))
	}
	var p struct {
		Completed float64 `json:"completed"`
		Total     float64 `json:"total"`
	}
	if json.Unmarshal(raw, &p) == nil && p.Total > 0 {
		return clampProgress(int(p.Completed * 100 / p.Total))
	}
	return 0
}

func clampProgress(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func videoCreated(raw json.RawMessage) int64 {
	if len(raw) > 0 {
		var n int64
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			return n
		}
	}
	return time.Now().Unix()
}

// videoRecord renders an OpenAI-style video object. detail is informational:
// size and seconds echo the submit when known.
func videoRecord(rec *sdcppJobRecord, model, size string, seconds int) map[string]any {
	status := videoStatus(rec.Status)
	doc := map[string]any{
		"id": rec.ID, "object": "video", "model": model, "status": status,
		"progress": videoProgress(rec.Progress), "created_at": videoCreated(rec.Created),
	}
	if size != "" {
		doc["size"] = size
	}
	if seconds > 0 {
		doc["seconds"] = seconds
	}
	if status == "failed" {
		switch {
		case strings.EqualFold(rec.Status, "cancelled"), strings.EqualFold(rec.Status, "canceled"):
			doc["error"] = map[string]any{"code": "cancelled", "message": "the job was cancelled"}
		case len(rec.Error) > 0 && string(rec.Error) != "null":
			var msg string
			if json.Unmarshal(rec.Error, &msg) == nil {
				doc["error"] = map[string]any{"code": "backend_error", "message": msg}
			} else {
				doc["error"] = rec.Error
			}
		case !strings.EqualFold(rec.Status, "failed") && !strings.EqualFold(rec.Status, "error"):
			// An unknown state is what failed the job; say which one it was.
			doc["error"] = map[string]any{"code": "backend_error", "message": "backend reported state " + rec.Status}
		}
	}
	return doc
}

// sdcppCreate translates a /v1/videos submit (JSON or multipart) into a
// vid_gen request, posts it, and writes the translated job record. The caller
// holds the response back so the job id is recorded before the client sees it.
func (r *Router) sdcppCreate(w http.ResponseWriter, req *http.Request, base, model string) {
	payload, size, seconds, err := videoCreatePayload(req)
	if err != nil {
		http.Error(w, "video adapter: "+err.Error(), http.StatusBadRequest)
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "video adapter: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, base+sdcppCreatePath, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "video adapter: "+err.Error(), http.StatusBadGateway)
		return
	}
	out.Header.Set("Content-Type", "application/json")
	resp, err := r.httpc.Do(out)
	if err != nil {
		http.Error(w, "video adapter: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		copyBackendError(w, resp)
		return
	}
	var rec sdcppJobRecord
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVideoJobBody)).Decode(&rec); err != nil || rec.ID == "" {
		http.Error(w, "video adapter: backend returned no job id", http.StatusBadGateway)
		return
	}
	writeJSON(w, videoRecord(&rec, model, size, seconds))
}

// videoCreatePayload reads the front-door submit and maps it onto sd-server's
// vid_gen fields. It also returns the echoed size and seconds for the reply.
func videoCreatePayload(req *http.Request) (map[string]any, string, int, error) {
	ct := req.Header.Get("Content-Type")
	var fields map[string]any
	if strings.Contains(ct, "multipart/form-data") {
		var err error
		if fields, err = readVideoMultipart(req); err != nil {
			return nil, "", 0, err
		}
	} else {
		dec := json.NewDecoder(req.Body)
		dec.UseNumber() // a large seed must reach the backend with its digits intact
		if err := dec.Decode(&fields); err != nil {
			return nil, "", 0, fmt.Errorf("decode submit: %w", err)
		}
	}
	return translateVideoFields(fields)
}

// readVideoMultipart streams the form: text fields become strings, file parts
// become raw base64 (sd-server's init_image/end_image shape). The global body
// cap already bounded the upload; this reads each part once.
func readVideoMultipart(req *http.Request) (map[string]any, error) {
	_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		return nil, fmt.Errorf("multipart: %v", err)
	}
	fields := map[string]any{}
	mr := multipart.NewReader(req.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return fields, nil
		}
		if err != nil {
			return nil, fmt.Errorf("multipart: %w", err)
		}
		name := part.FormName()
		if name == "" {
			part.Close()
			continue
		}
		if part.FileName() != "" {
			data, err := io.ReadAll(part)
			part.Close()
			if err != nil {
				return nil, fmt.Errorf("multipart %s: %w", name, err)
			}
			fields[name] = base64.StdEncoding.EncodeToString(data)
			continue
		}
		val, err := io.ReadAll(io.LimitReader(part, 64<<10))
		part.Close()
		if err != nil {
			return nil, fmt.Errorf("multipart %s: %w", name, err)
		}
		fields[name] = string(val)
	}
}

// translateVideoFields maps the OpenAI/vLLM video names onto sd-server's
// vid_gen JSON. Unknown fields are dropped; the model is the stanza's, so it
// is not sent.
func translateVideoFields(in map[string]any) (map[string]any, string, int, error) {
	out := map[string]any{}
	for _, key := range []string{"prompt", "negative_prompt", "output_format", "output_compression",
		"end_image", "ref_images", "control_frames", "vace_strength", "ref_videos",
		"ref_audios", "ref_video_audios"} {
		if v, ok := in[key]; ok {
			out[key] = v
		}
	}
	// input_reference (multipart file or JSON string) is sd-server's init_image.
	if v, ok := in["input_reference"]; ok && v != "" && v != nil {
		out["init_image"] = v
	}
	if v, ok := in["init_image"]; ok {
		out["init_image"] = v
	}

	fps := intField(in, "fps")
	if fps <= 0 {
		fps = 24
	}
	frames := intField(in, "num_frames")
	if frames <= 0 {
		frames = intField(in, "video_frames")
	}
	seconds := intField(in, "seconds")
	if seconds <= 0 {
		seconds = 4
	}
	if frames <= 0 {
		frames = seconds * fps
	}
	if frames < 1 {
		frames = 1
	}
	out["fps"] = fps
	out["video_frames"] = frames
	// Keep seconds derived from the frame count so a reply matches what runs.
	seconds = (frames + fps - 1) / fps

	size := ""
	width, height := intField(in, "width"), intField(in, "height")
	if s, ok := in["size"].(string); ok && s != "" {
		size = s
		if w, h, ok := parseVideoSize(s); ok {
			width, height = w, h
		}
	}
	if width > 0 && height > 0 {
		out["width"], out["height"] = width, height
		if size == "" {
			size = fmt.Sprintf("%dx%d", width, height)
		}
	}
	if v, ok := in["seed"]; ok {
		out["seed"] = v
	}
	// Sampling parameters live under sample_params in sd-server's /sdcpp/v1
	// request shape: a flat "steps" is accepted and silently ignored for
	// vid_gen, so the job runs at the recipe's default step count instead of
	// the one the client asked for. cfg_scale is guidance.txt_cfg.
	sp := map[string]any{}
	if v := intField(in, "num_inference_steps"); v > 0 {
		sp["sample_steps"] = v
	} else if v := intField(in, "steps"); v > 0 {
		sp["sample_steps"] = v
	}
	if v, ok := in["cfg_scale"]; ok {
		sp["guidance"] = map[string]any{"txt_cfg": v}
	}
	if v, ok := in["scheduler"]; ok {
		sp["scheduler"] = v
	}
	if len(sp) > 0 {
		out["sample_params"] = sp
	}
	return out, size, seconds, nil
}

// parseVideoSize reads OpenAI's "WxH".
func parseVideoSize(s string) (int, int, bool) {
	w, h, ok := strings.Cut(s, "x")
	if !ok {
		return 0, 0, false
	}
	width, errW := strconv.Atoi(strings.TrimSpace(w))
	height, errH := strconv.Atoi(strings.TrimSpace(h))
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// intField reads a number that may arrive as a JSON number or a form string.
func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}
	return 0
}

func sdcppJobURL(base, id, suffix string) string {
	return base + sdcppJobsPath + url.PathEscape(id) + suffix
}

// sdcppFetch GETs one job record and decodes it into v. found is false on
// the backend's 404.
func (r *Router) sdcppFetch(ctx context.Context, base, id string, v any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sdcppJobURL(base, id, ""), nil)
	if err != nil {
		return false, err
	}
	resp, err := r.httpc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("job %s: %s", id, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVideoJobContent)).Decode(v); err != nil {
		return false, err
	}
	return true, nil
}

// sdcppGet fetches one record without its result. found is false on the
// backend's 404.
func (r *Router) sdcppGet(ctx context.Context, base, id string) (*sdcppJobRecord, bool, error) {
	var out sdcppJobRecord
	found, err := r.sdcppFetch(ctx, base, id, &out)
	if !found || err != nil {
		return nil, found, err
	}
	return &out, true, nil
}

// pollSDCppJob reports the mapped status for the router's own watcher.
func (r *Router) pollSDCppJob(ctx context.Context, base, id string) (string, error) {
	rec, found, err := r.sdcppGet(ctx, base, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "gone", nil
	}
	return videoStatus(rec.Status), nil
}

// serveSDCppFollow answers a status, content or cancel call for one job.
// deleteOK reports that a DELETE reached a 2xx so the caller can end the hold.
func (r *Router) serveSDCppFollow(w http.ResponseWriter, req *http.Request, base, model, id string) (deleteOK bool) {
	ctx := req.Context()
	if req.Method == http.MethodDelete {
		out, err := http.NewRequestWithContext(ctx, http.MethodPost, sdcppJobURL(base, id, "/cancel"), nil)
		if err != nil {
			http.Error(w, "video adapter: "+err.Error(), http.StatusBadGateway)
			return false
		}
		resp, err := r.httpc.Do(out)
		if err != nil {
			http.Error(w, "video adapter: "+err.Error(), http.StatusBadGateway)
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			copyBackendError(w, resp)
			return false
		}
		writeJSON(w, map[string]any{"id": id, "deleted": true})
		return true
	}
	if req.Method == http.MethodPost {
		http.Error(w, "remix is not supported by the sdcpp adapter", http.StatusNotImplemented)
		return false
	}

	if strings.HasSuffix(req.URL.Path, "/content") {
		var rec sdcppJobContent
		found, err := r.sdcppFetch(ctx, base, id, &rec)
		if err != nil {
			http.Error(w, "video adapter: "+err.Error(), http.StatusBadGateway)
			return false
		}
		if !found {
			http.Error(w, fmt.Sprintf("unknown job %q on the backend", id), http.StatusNotFound)
			return false
		}
		if videoStatus(rec.Status) != "completed" {
			http.Error(w, "job not completed yet", http.StatusConflict)
			return false
		}
		if rec.Result == nil || rec.Result.B64JSON == "" {
			http.Error(w, "video adapter: backend completed without content", http.StatusBadGateway)
			return false
		}
		if wantsMP4(req) {
			// sd-server writes webm/webp/avi only, and a WebM carrying PCM
			// audio plays silent in browsers (upstream #2233). A client that
			// asks for mp4 gets H.264 + AAC.
			if err := writeMP4(ctx, w, rec.Result.B64JSON, videoFPS(req)); err != nil {
				http.Error(w, "video adapter: "+err.Error(), http.StatusBadGateway)
			}
			return false
		}
		ct := rec.Result.MimeType
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		// shortcut: sd-server audio in WebM is passed through unchanged, remux with ffmpeg when a client needs browser-playable audio.
		_, _ = io.Copy(w, base64.NewDecoder(base64.StdEncoding, strings.NewReader(rec.Result.B64JSON)))
		return false
	}
	rec, found, err := r.sdcppGet(ctx, base, id)
	if err != nil {
		http.Error(w, "video adapter: "+err.Error(), http.StatusBadGateway)
		return false
	}
	if !found {
		http.Error(w, fmt.Sprintf("unknown job %q on the backend", id), http.StatusNotFound)
		return false
	}
	writeJSON(w, videoRecord(rec, model, "", 0))
	return false
}

// copyBackendError forwards a backend error status and a bounded body.
func copyBackendError(w http.ResponseWriter, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxVideoJobBody))
}

// wantsMP4 reports that the client asked for a browser-playable MP4 instead of
// the backend's own container: ?format=mp4, or an Accept header naming it.
func wantsMP4(req *http.Request) bool {
	switch f := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("format"))); f {
	case "mp4":
		return true
	case "":
	default:
		return false
	}
	return strings.Contains(strings.ToLower(req.Header.Get("Accept")), "video/mp4")
}

// videoFPS reads the ?fps= override a client may send with an mp4 fetch.
func videoFPS(req *http.Request) int {
	n, _ := strconv.Atoi(strings.TrimSpace(req.URL.Query().Get("fps")))
	if n < 1 || n > 120 {
		return 0
	}
	return n
}

// writeMP4 transcodes a backend clip to MP4 (H.264 + AAC) with a
// Content-Length. The work lands in a temp file first, so an ffmpeg failure
// still produces an error status instead of a half-written body. fps > 0
// forces a constant frame rate: sd-server's WebM muxer writes frames without
// timestamps for some short clips (a 9-frame clip reported 0.001 s and
// collapsed to one frame), and -fflags +genpts alone does not recover them.
func writeMP4(ctx context.Context, w http.ResponseWriter, b64 string, fps int) error {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("mp4 requested but ffmpeg is not installed")
	}
	tmp, err := os.CreateTemp("", "mr-video-*.mp4")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	args := []string{"-v", "error", "-fflags", "+genpts", "-i", "pipe:0"}
	if fps > 0 {
		args = append(args, "-fps_mode", "cfr", "-r", strconv.Itoa(fps))
	}
	// Write to the temp file by path, not pipe:1: ffmpeg treats "pipe:" as a
	// non-seekable output and then refuses +faststart (and any moov rewrite).
	args = append(args,
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "veryfast", "-crf", "20",
		"-c:a", "aac", "-b:a", "128k",
		"-movflags", "+faststart", "-f", "mp4", "-y", tmp.Name())
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = base64.NewDecoder(base64.StdEncoding, strings.NewReader(b64))
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	st, err := tmp.Stat()
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	_, err = io.Copy(w, tmp)
	return err
}
