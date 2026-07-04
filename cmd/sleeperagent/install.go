package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

func installCmd(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory to install sleeperagent into")
	force := fs.Bool("force", false, "overwrite an existing different file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	src, err := os.Executable()
	if err != nil {
		return err
	}
	targetDir := *dir
	if targetDir == "" {
		targetDir, err = defaultInstallDir()
		if err != nil {
			return err
		}
	}
	targetDir, err = filepath.Abs(targetDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}

	target := filepath.Join(targetDir, installName(runtime.GOOS))
	status, err := installExecutable(src, target, *force)
	if err != nil {
		return err
	}
	fmt.Printf("sleeperagent install: %s: %s\n", status, target)
	if pathContainsDir(os.Getenv("PATH"), targetDir, runtime.GOOS) {
		fmt.Println("Open a new shell, then run: sleeperagent version")
		return nil
	}
	fmt.Printf("sleeperagent install: %s is not on PATH yet.\n", targetDir)
	fmt.Println(pathRemediation(targetDir, runtime.GOOS))
	fmt.Println("Open a new shell after updating PATH, then run: sleeperagent version")
	return nil
}

func defaultInstallDir() (string, error) {
	if runtime.GOOS == "windows" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "Microsoft", "WindowsApps"), nil
		}
		return "", fmt.Errorf("LOCALAPPDATA is not set; pass --dir DIR")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

func installName(goos string) string {
	if goos == "windows" {
		return "sleeperagent.exe"
	}
	return "sleeperagent"
}

func installExecutable(src, target string, force bool) (string, error) {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	if existing, err := os.Stat(target); err == nil {
		same, serr := sameExecutable(src, target, srcInfo, existing)
		if serr != nil {
			return "", serr
		}
		if same {
			return "already installed", nil
		}
		if !force {
			return "", fmt.Errorf("%s already exists and is different; rerun with --force to overwrite it", target)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	// Clean up any binary sidelined by a previous in-place update (see below).
	aside := target + ".old"
	_ = os.Remove(aside)

	tmp := target + ".tmp"
	if err := copyFile(src, tmp, srcInfo.Mode()|0o755); err != nil {
		return "", err
	}

	// Fast path: move straight into place. Works when the target is absent or not
	// locked (no running instance is using it).
	if err := os.Rename(tmp, target); err == nil {
		return "installed", nil
	}

	// In-use path: Windows locks a running .exe against overwrite/delete, but it
	// can still be renamed. Move the old binary aside, then the new one into
	// place, so you can update while an instance is still being watched. The
	// sidelined copy is removed on the next install, once the old process exits.
	if err := os.Rename(target, aside); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("replace %s (is it in use? close other sleeperagent processes or pass a fresh --dir): %w", target, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Rename(aside, target) // restore the original on failure
		_ = os.Remove(tmp)
		return "", fmt.Errorf("install to %s: %w", target, err)
	}
	_ = os.Remove(aside) // best-effort; harmless if the old binary is still locked
	return "installed", nil
}

func sameExecutable(src, target string, srcInfo, targetInfo os.FileInfo) (bool, error) {
	if os.SameFile(srcInfo, targetInfo) {
		return true, nil
	}
	return filesEqual(src, target)
}

func filesEqual(a, b string) (bool, error) {
	ab, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ab, bb), nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// pathContainsDir and cleanPathForCompare take an explicit goos so they can
// evaluate a PATH string for any target OS, not just the one this binary was
// built for. path/filepath's separator conventions are fixed at build time to
// runtime.GOOS, so filepath.SplitList/filepath.Clean silently apply the wrong
// rules whenever goos != the build OS (e.g. checking Windows PATH semantics
// from a Linux/macOS test binary) — hence the hand-rolled logic below.
func pathContainsDir(pathValue, dir, goos string) bool {
	if pathValue == "" {
		return false
	}
	want := cleanPathForCompare(dir, goos)
	listSep := ":"
	if goos == "windows" {
		listSep = ";"
	}
	for _, entry := range strings.Split(pathValue, listSep) {
		if cleanPathForCompare(entry, goos) == want {
			return true
		}
	}
	return false
}

func cleanPathForCompare(p, goos string) string {
	if goos == "windows" {
		cleaned := path.Clean(strings.ReplaceAll(p, `\`, "/"))
		return strings.ToLower(strings.ReplaceAll(cleaned, "/", `\`))
	}
	return path.Clean(p)
}

func pathRemediation(dir, goos string) string {
	if goos == "windows" {
		return fmt.Sprintf("Add it for future shells with:\n  setx PATH \"%%PATH%%;%s\"\nOr add it in Settings > System > About > Advanced system settings > Environment Variables.", dir)
	}
	return fmt.Sprintf("Add it for future shells with:\n  export PATH=\"$PATH:%s\"\nPut that line in your shell profile if you want it to persist.", dir)
}
