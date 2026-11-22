package router

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Image tab has three modes (#200). Generate keeps txt2img; Edit and
// Outpaint are img2img, and Outpaint carries an `outpaint` object the router
// resolves into a padded canvas plus mask.
func TestEmbeddedUIImageTabModes(t *testing.T) {
	for _, want := range []string{
		`id="image-mode"`,
		`<option value="generate" selected>`,
		`<option value="edit">`,
		`<option value="outpaint">`,
		`id="image-source"`, // picker
		`id="image-drop"`,   // drag and drop
		`id="image-source-preview"`,
		`id="image-strength"`, // edit denoise
		`id="image-cfg"`,      // shared CFG, also used by Generate
		`id="image-batch"`,    // shared batch count
		`id="image-seed-random"`,
		`id="image-preset"`, // 1.5x / 2x / 1:1 / 16:9 / 9:16
		`id="image-pad-top"`,
		`id="image-outpaint-preview"`,
		`id="image-outpaint-marker"`, // the new area is marked
		`id="image-download"`,
		`id="image-use-source"`,
		`id="image-mode-note"`, // explains a disabled mode
	} {
		if !strings.Contains(webUIHTML, want) {
			t.Errorf("image tab missing %q", want)
		}
	}
	// The old single-mode tab had no mode control and no outpaint field.
	if strings.Contains(webUIHTML, "Generate through the sd.cpp compatibility route") {
		t.Error("image tab still carries the old single-mode heading")
	}
}

// The request builder must produce the three shapes: txt2img for Generate,
// img2img with init_images for Edit, and img2img with an outpaint object for
// Outpaint. This is a source check because the builder is browser code; the pad
// and mask maths it feeds is covered by Go tests in outpaint_test.go.
func TestEmbeddedUIImageRequestBuilder(t *testing.T) {
	js := uiAppJS
	for _, want := range []string{
		"window.imageRequestBody=function(mode)",
		"'/sdapi/v1/txt2img':'/sdapi/v1/img2img'", // path per mode
		"body.init_images=[imageSource.data]",
		"body.denoising_strength=Number($('image-strength').value)",
		"body.denoising_strength=Number($('image-fill-strength').value)",
		"body.outpaint=preset?{preset}:outpaintMargins()",
		"cfg_scale:Number($('image-cfg').value)",
		"batch_size:Number($('image-batch').value)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("image request builder missing %q", want)
		}
	}
	// Generate must stay on txt2img with a plain size, not img2img fields.
	gen := js[strings.Index(js, "if(mode==='generate'){"):]
	if !strings.Contains(gen[:600], "body.width=Number($('image-width').value)") {
		t.Error("generate body must set width/height")
	}
}

// TestImageGatingRule runs the gating rule under node against fixture
// capabilities: Edit needs img2img, Outpaint needs inpaint, and a mode the
// model cannot serve falls back to Generate with a reason.
func TestImageGatingRule(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the gating rule is exercised in the browser")
	}
	start := strings.Index(uiAppJS, "// --- image gating rule")
	end := strings.Index(uiAppJS, "// --- end image gating rule ---")
	if start < 0 || end < 0 {
		t.Fatal("gating rule markers are missing from app.js")
	}
	rule := uiAppJS[start:end]
	harness := "globalThis.window = globalThis;\n" + rule + `
const cases = [
  {caps:['img2img','inpaint'], mode:'edit',    want:'edit',    edit:true,  outpaint:true,  note:false},
  {caps:['img2img','inpaint'], mode:'outpaint',want:'outpaint',edit:true,  outpaint:true,  note:false},
  {caps:['img2img'],           mode:'outpaint',want:'generate',edit:true,  outpaint:false, note:true},
  {caps:['inpaint'],           mode:'edit',    want:'generate',edit:false, outpaint:true,  note:true},
  {caps:[],                    mode:'edit',    want:'generate',edit:false, outpaint:false, note:true},
  {caps:[],                    mode:'generate',want:'generate',edit:false, outpaint:false, note:false},
];
let bad = 0;
for (const c of cases) {
  const st = imageModeState(c.caps, c.mode);
  const ok = st.mode === c.want && st.editOk === c.edit && st.outpaintOk === c.outpaint && (!!st.note === c.note);
  if (!ok) { bad++; console.error('FAIL', JSON.stringify(c), '->', JSON.stringify(st)); }
}
if (bad) process.exit(1);
`
	dir := t.TempDir()
	file := filepath.Join(dir, "gating.js")
	if err := os.WriteFile(file, []byte(harness), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	out, runErr := exec.Command(node, file).CombinedOutput()
	if runErr != nil {
		t.Fatalf("gating rule failed: %v\n%s", runErr, out)
	}
}

// TestImageCapsDoesNotInheritOtherModels is the #203 gate: a selected model
// without img2img must not inherit the flag from another image model, and a
// path_default stanza with no flags must not fall back to the union either.
func TestImageCapsDoesNotInheritOtherModels(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the caps picker is exercised in the browser")
	}
	start := strings.Index(uiAppJS, "// --- image caps picker")
	end := strings.Index(uiAppJS, "// --- end image caps picker ---")
	if start < 0 || end < 0 {
		t.Fatal("image caps picker markers are missing from app.js")
	}
	rule := uiAppJS[start:end]
	harness := "globalThis.window = globalThis;\n" + rule + `
const models = [
  {id:'edit-ok', api_type:'image', capabilities:['img2img']},
  {id:'gen-only', api_type:'image'},
  {id:'gen-empty', api_type:'image', capabilities:[]},
  {id:'path-def', api_type:'image', path_default:true},
  {id:'chat', api_type:'chat', capabilities:['vision']},
];
const cases = [
  {id:'gen-only', want:[]},
  {id:'gen-empty', want:[]},
  {id:'edit-ok', want:['img2img']},
  {id:'path-def', want:[]},
  {id:'', want:[]}, // Path default follows path-def, which has no flags
  {id:'missing', want:[]},
];
let bad = 0;
for (const c of cases) {
  const got = imageCapsFor(models, c.id);
  const ok = JSON.stringify(got) === JSON.stringify(c.want);
  if (!ok) { bad++; console.error('FAIL', JSON.stringify(c), '->', JSON.stringify(got)); }
}
// Union only when Path default has no path_default stanza at all.
const noDefault = models.filter(m => m.id !== 'path-def');
const union = imageCapsFor(noDefault, '');
if (JSON.stringify(union) !== JSON.stringify(['img2img'])) {
  bad++; console.error('FAIL union', JSON.stringify(union));
}
if (bad) process.exit(1);
`
	dir := t.TempDir()
	file := filepath.Join(dir, "caps.js")
	if err := os.WriteFile(file, []byte(harness), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	out, runErr := exec.Command(node, file).CombinedOutput()
	if runErr != nil {
		t.Fatalf("image caps picker failed: %v\n%s", runErr, out)
	}
}
