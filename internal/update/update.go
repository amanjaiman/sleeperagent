// Package update implements the self-update flow: discover the newest GitHub
// release, compare it to the running version, and swap the executable for the
// freshly downloaded (checksum-verified) release binary. Checks are throttled
// through a small cache file so startup never spams the network.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/amanjaiman/sleeperagent/internal/statefile"
)

// repoPath is the GitHub owner/name whose releases we track.
const repoPath = "amanjaiman/sleeperagent"

// binaryName is the executable inside a release archive.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "sleeperagent.exe"
	}
	return "sleeperagent"
}

// Client talks to the release host. BaseURL is normally https://github.com;
// SLEEPERAGENT_UPDATE_BASE_URL overrides it (tests, mirrors).
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a Client with sane timeouts.
func New() *Client {
	base := "https://github.com"
	if u := os.Getenv("SLEEPERAGENT_UPDATE_BASE_URL"); u != "" {
		base = strings.TrimRight(u, "/")
	}
	return &Client{BaseURL: base, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// LatestVersion returns the newest release tag (e.g. "v0.5.0"). It reads the
// redirect target of /releases/latest, which needs no API token and is not
// rate-limited like the REST API.
func (c *Client) LatestVersion(ctx context.Context) (string, error) {
	url := c.BaseURL + "/" + repoPath + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// Don't follow the redirect — its Location IS the answer.
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		return "", fmt.Errorf("expected a redirect from %s, got %s", url, resp.Status)
	}
	tag := path.Base(loc)
	if _, _, _, ok := parseVersion(tag); !ok {
		return "", fmt.Errorf("release redirect points at %q, which does not look like a version tag", loc)
	}
	return tag, nil
}

// Parseable reports whether v looks like a release version ("0.5.0",
// "v0.5.0"). Source builds report "dev" (or a pseudo-version) and can't be
// meaningfully compared to releases.
func Parseable(v string) bool {
	_, _, _, ok := parseVersion(v)
	return ok
}

// Newer reports whether latest is strictly newer than current. Unparseable
// versions (e.g. a "dev" source build) are never considered outdated.
func Newer(current, latest string) bool {
	cmaj, cmin, cpat, ok := parseVersion(current)
	if !ok {
		return false
	}
	lmaj, lmin, lpat, ok := parseVersion(latest)
	if !ok {
		return false
	}
	if lmaj != cmaj {
		return lmaj > cmaj
	}
	if lmin != cmin {
		return lmin > cmin
	}
	return lpat > cpat
}

// parseVersion accepts "v1.2.3", "1.2.3", or "1.2" (patch 0); anything after a
// "-" or "+" (pre-release/build metadata) is ignored.
func parseVersion(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// AssetName is the release archive for this OS/arch, e.g.
// sleeperagent_0.5.0_darwin_arm64.tar.gz (zip on Windows).
func AssetName(version string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("sleeperagent_%s_%s_%s.%s",
		strings.TrimPrefix(version, "v"), runtime.GOOS, runtime.GOARCH, ext)
}

// Apply downloads the given release, verifies it against checksums.txt, and
// replaces the running executable. The running process keeps executing the old
// code; the new version takes effect on the next start.
func (c *Client) Apply(ctx context.Context, version string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return c.ApplyTo(ctx, version, exe)
}

// ApplyTo is Apply targeting an explicit path (separated out for tests).
func (c *Client) ApplyTo(ctx context.Context, version, target string) error {
	asset := AssetName(version)
	base := c.BaseURL + "/" + repoPath + "/releases/download/" + version + "/"

	archive, err := c.download(ctx, base+asset)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	sums, err := c.download(ctx, base+"checksums.txt")
	if err != nil {
		return fmt.Errorf("download checksums.txt: %w", err)
	}
	if err := verifyChecksum(sums, asset, archive); err != nil {
		return err
	}
	bin, err := extractBinary(asset, archive)
	if err != nil {
		return err
	}
	return replaceExecutable(target, bin)
}

func (c *Client) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// verifyChecksum checks data against the asset's line in a goreleaser
// checksums.txt ("<sha256hex>  <filename>" per line).
func verifyChecksum(sums []byte, asset string, data []byte) error {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != asset {
			continue
		}
		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != strings.ToLower(fields[0]) {
			return fmt.Errorf("checksum mismatch for %s — refusing to install", asset)
		}
		return nil
	}
	return fmt.Errorf("no checksum entry for %s in checksums.txt", asset)
}

// extractBinary pulls the sleeperagent executable out of a release archive.
func extractBinary(asset string, archive []byte) ([]byte, error) {
	want := binaryName()
	if strings.HasSuffix(asset, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("open zip: %w", err)
		}
		for _, f := range zr.File {
			if path.Base(f.Name) != want {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
		return nil, fmt.Errorf("%s not found in %s", want, asset)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open tar.gz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in %s", want, asset)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == want {
			return io.ReadAll(tr)
		}
	}
}

// replaceExecutable swaps target for the new binary. On Unix a rename over the
// old path is atomic and safe while the old binary is running. Windows can't
// overwrite a running exe, so the running one is renamed aside to
// "<target>.old" first (cleaned up on the next start; see CleanupOld).
func replaceExecutable(target string, bin []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".sleeperagent-update-*")
	if err != nil {
		return fmt.Errorf("stage new binary (is %s writable?): %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		old := target + ".old"
		_ = os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return fmt.Errorf("move running executable aside: %w", err)
		}
		if err := os.Rename(tmpName, target); err != nil {
			// Try to put the old binary back so the install isn't left broken.
			_ = os.Rename(old, target)
			return fmt.Errorf("install new executable: %w", err)
		}
		return nil
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("install new executable: %w", err)
	}
	return nil
}

// CleanupOld removes the "<exe>.old" left behind by a Windows self-update.
// Best-effort: it fails silently while an old instance still runs.
func CleanupOld() {
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

// checkCache throttles the startup update check.
type checkCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

func cachePath() string { return filepath.Join(statefile.Dir(), "update-check.json") }

// ShouldCheck reports whether the last check is older than maxAge. A missing
// or unreadable cache means "check now".
func ShouldCheck(maxAge time.Duration) bool {
	b, err := os.ReadFile(cachePath())
	if err != nil {
		return true
	}
	var c checkCache
	if json.Unmarshal(b, &c) != nil {
		return true
	}
	return time.Since(c.CheckedAt) >= maxAge
}

// RecordCheck persists the result (or attempt) of an update check so the next
// startup within maxAge skips the network entirely.
func RecordCheck(latest string) {
	b, err := json.Marshal(checkCache{CheckedAt: time.Now(), Latest: latest})
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(cachePath()), 0o755)
	_ = os.WriteFile(cachePath(), b, 0o644)
}
