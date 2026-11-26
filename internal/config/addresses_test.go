package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Tracked files carry placeholder addresses (#184/#195): the LAN /24 as
// 10.99.0.x and tailnet hosts as 203.0.113.x (TEST-NET-3, which this check does
// not match). Real node values live in the untracked overlay at
// <config dir>/local/<node>.yaml. This test fails when a real RFC1918 or CGNAT
// literal comes back into a tracked file, so a deploy from git cannot point a
// node at somebody's actual box.

var privateAddrRe = regexp.MustCompile(`\b(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3})\b`)

// Documented generic range constants and the placeholder LAN are allowed.
var allowedLiterals = map[string]bool{
	"10.0.0.0":    true, // 10.0.0.0/8, the RFC1918 constant in access.go
	"172.16.0.0":  true, // 172.16.0.0/12
	"192.168.0.0": true, // 192.168.0.0/16
	"100.64.0.0":  true, // 100.64.0.0/10, the CGNAT constant
}

// Files that must contain a private literal to test access control. Keep this
// list short and give the reason.
var allowedFiles = map[string]string{
	"internal/router/access_test.go":      "CGNAT fixtures proving the trusted-proxy rules",
	"internal/router/reload_http_test.go": "CGNAT fixture proving LAN clients bypass the key requirement",
}

func placeholderLAN(addr string) bool {
	return strings.HasPrefix(addr, "10.99.")
}

// repoRoot resolves the checkout from the package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return root
}

func TestTrackedFilesCarryNoRealAddresses(t *testing.T) {
	root := repoRoot(t)
	skipDirs := map[string]bool{".git": true, "build": true, "node_modules": true, ".pids": true}
	var findings []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			// Overlay dir is gitignored and holds real addresses after
			// deploy/overlay-split.py. CI never has it; a local run might.
			if filepath.ToSlash(rel) == "configs/local" {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Size() > 2<<20 {
			return nil // binaries and generated blobs
		}
		switch filepath.Ext(path) {
		case ".png", ".jpg", ".jpeg", ".webp", ".gguf", ".mp4", ".wav", ".gif", ".ico", ".zip", ".gz", ".zst":
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.ContainsRune(string(body), 0) {
			return nil // binary
		}
		allowFile := allowedFiles[filepath.ToSlash(rel)] != ""
		for i, line := range strings.Split(string(body), "\n") {
			for _, m := range privateAddrRe.FindAllString(line, -1) {
				if allowedLiterals[m] || placeholderLAN(m) {
					continue
				}
				if allowFile {
					continue
				}
				findings = append(findings, rel+":"+itoa(i+1)+": "+m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(findings) > 0 {
		t.Fatalf("real private addresses in tracked files (use the 10.99.x / 203.0.113.x placeholders, and keep the real value in configs/local/<node>.yaml, which is gitignored):\n  %s",
			strings.Join(findings, "\n  "))
	}
}

// TestOverlayPathIsGitignored keeps the overlay contract honest: the overlay
// directory must never be committable.
func TestOverlayPathIsGitignored(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(body), "configs/local/") {
		t.Fatal(".gitignore must ignore configs/local/ so a per-node overlay cannot be committed")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
