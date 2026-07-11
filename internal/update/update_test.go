package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.4.0", "v0.5.0", true},
		{"v0.4.0", "v0.4.1", true},
		{"0.4.0", "v1.0.0", true},
		{"0.4.0", "v0.4.0", false},
		{"0.5.0", "v0.4.9", false},
		{"1.0.0", "v0.9.9", false},
		{"dev", "v0.5.0", false},              // source build: never nag
		{"0.4.0", "some-tag", false},          // garbage latest
		{"0.4.0", "v0.4", false},              // 0.4 == 0.4.0
		{"0.4.0", "v0.5.0-rc1", true},         // pre-release suffix ignored
		{"0.4.0-12-gdeadbee", "v0.4.1", true}, // git-describe style current
	}
	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestParseable(t *testing.T) {
	for v, want := range map[string]bool{
		"0.4.0": true, "v0.5.0": true, "0.4": true,
		"dev": false, "": false, "abc.def": false,
	} {
		if got := Parseable(v); got != want {
			t.Errorf("Parseable(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestAssetName(t *testing.T) {
	got := AssetName("v0.5.0")
	wantPrefix := fmt.Sprintf("sleeperagent_0.5.0_%s_%s.", runtime.GOOS, runtime.GOARCH)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("AssetName = %q, want prefix %q", got, wantPrefix)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(got, ".zip") {
		t.Fatalf("windows asset should be a zip, got %q", got)
	}
	if runtime.GOOS != "windows" && !strings.HasSuffix(got, ".tar.gz") {
		t.Fatalf("non-windows asset should be a tar.gz, got %q", got)
	}
}

func TestLatestVersionFollowsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+repoPath+"/releases/latest" {
			http.Redirect(w, r, srv_url(r)+"/"+repoPath+"/releases/tag/v9.9.9", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cl := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := cl.LatestVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "v9.9.9" {
		t.Fatalf("LatestVersion = %q, want v9.9.9", got)
	}
}

// srv_url reconstructs the test server's base URL from the request, so the
// redirect Location is absolute like GitHub's.
func srv_url(r *http.Request) string { return "http://" + r.Host }

func TestLatestVersionRejectsNonVersionRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/login", http.StatusFound)
	}))
	defer srv.Close()
	cl := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := cl.LatestVersion(context.Background()); err == nil {
		t.Fatal("expected an error for a redirect that is not a version tag")
	}
}

// makeArchive builds a release archive for the current platform containing the
// platform's binary name with the given contents.
func makeArchive(t *testing.T, contents []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		f, err := zw.Create(binaryName())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(contents); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: binaryName(), Mode: 0o755, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestApplyToReplacesBinary(t *testing.T) {
	newBin := []byte("new binary contents")
	archive := makeArchive(t, newBin)
	asset := AssetName("v9.9.9")
	sum := sha256.Sum256(archive)
	checksums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), binaryName())
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	cl := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := cl.ApplyTo(context.Background(), "v9.9.9", target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBin) {
		t.Fatalf("target = %q, want the new binary contents", got)
	}
	if runtime.GOOS == "windows" {
		// The old exe is parked as .old (deleted on next start by CleanupOld).
		if _, err := os.Stat(target + ".old"); err != nil {
			t.Fatalf("windows update should leave %s.old behind: %v", target, err)
		}
	}
}

func TestApplyToRejectsBadChecksum(t *testing.T) {
	archive := makeArchive(t, []byte("evil"))
	asset := AssetName("v9.9.9")
	checksums := strings.Repeat("0", 64) + "  " + asset + "\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), binaryName())
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	cl := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := cl.ApplyTo(context.Background(), "v9.9.9", target); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch error, got %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old binary" {
		t.Fatal("a failed update must not touch the existing binary")
	}
}

func TestApplyToRejectsMissingChecksumEntry(t *testing.T) {
	archive := makeArchive(t, []byte("x"))
	asset := AssetName("v9.9.9")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			w.Write([]byte("deadbeef  something_else.tar.gz\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	target := filepath.Join(t.TempDir(), binaryName())
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	cl := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := cl.ApplyTo(context.Background(), "v9.9.9", target); err == nil || !strings.Contains(err.Error(), "no checksum entry") {
		t.Fatalf("want missing-entry error, got %v", err)
	}
}

func TestShouldCheckThrottles(t *testing.T) {
	t.Setenv("SLEEPERAGENT_STATE_DIR", t.TempDir())
	if !ShouldCheck(24 * time.Hour) {
		t.Fatal("no cache yet: should check")
	}
	RecordCheck("v0.5.0")
	if ShouldCheck(24 * time.Hour) {
		t.Fatal("fresh cache: should not check again")
	}
	if !ShouldCheck(0) {
		t.Fatal("zero max age: should always check")
	}
}
