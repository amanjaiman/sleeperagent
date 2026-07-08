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
	noProfile := fs.Bool("no-profile", false, "don't modify shell profile files; just print the PATH line")
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
	if *noProfile {
		fmt.Println(pathRemediation(targetDir, runtime.GOOS))
	} else if profile, updated, err := ensurePathInShellProfile(targetDir, runtime.GOOS); err == nil && profile != "" {
		if updated {
			fmt.Printf("Added it for future shells in %s.\n", profile)
		} else {
			fmt.Printf("A PATH update for this directory already exists in %s.\n", profile)
		}
	} else {
		if err != nil {
			fmt.Printf("Could not update your shell profile automatically: %v\n", err)
		}
		fmt.Println(pathRemediation(targetDir, runtime.GOOS))
	}
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
	return fmt.Sprintf("Add it for future shells with:\n  %s\nPut that line in your shell profile if you want it to persist.", shellPathLine(dir))
}

func ensurePathInShellProfile(dir, goos string) (string, bool, error) {
	if goos == "windows" {
		return "", false, nil
	}
	profile, err := defaultShellProfile(goos)
	if err != nil {
		return "", false, err
	}
	if profile == "" {
		// Unrecognized shell (fish, nushell, …) — an `export PATH=…` line would
		// be wrong or unread there, so leave it to the printed remediation.
		return "", false, nil
	}
	content, err := os.ReadFile(profile)
	if err != nil && !os.IsNotExist(err) {
		return profile, false, err
	}
	if profileMentionsDir(string(content), dir) {
		return profile, false, nil
	}
	block := "# Added by sleeperagent install.\n" + shellPathLine(dir) + "\n"
	f, err := os.OpenFile(profile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return profile, false, err
	}
	defer f.Close()
	if len(content) > 0 && content[len(content)-1] != '\n' {
		block = "\n" + block
	}
	if _, err := f.WriteString(block); err != nil {
		return profile, false, err
	}
	return profile, true, nil
}

func defaultShellProfile(goos string) (string, error) {
	if goos != "windows" {
		if home := os.Getenv("HOME"); home != "" {
			return shellProfileInHome(home, os.Getenv("SHELL"), goos), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return shellProfileInHome(home, os.Getenv("SHELL"), goos), nil
}

// profileMentionsDir reports whether an uncommented line of the profile
// already references dir, so a commented-out or disabled entry doesn't count
// as coverage.
func profileMentionsDir(content, dir string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, dir) {
			return true
		}
	}
	return false
}

// shellProfileInHome returns the profile file the user's shell actually reads
// on new terminals, or "" for shells where an `export PATH` line wouldn't work
// (fish and friends).
func shellProfileInHome(home, shell, goos string) string {
	switch filepath.Base(shell) {
	case "zsh":
		if goos == "darwin" {
			return filepath.Join(home, ".zprofile")
		}
		return filepath.Join(home, ".zshrc")
	case "bash":
		if goos == "darwin" {
			return filepath.Join(home, ".bash_profile")
		}
		// Linux terminal emulators open interactive non-login shells, which
		// read .bashrc; .profile is skipped entirely once .bash_profile exists.
		return filepath.Join(home, ".bashrc")
	case "sh", "", ".":
		if goos == "darwin" {
			return filepath.Join(home, ".zprofile")
		}
		return filepath.Join(home, ".profile")
	default:
		return ""
	}
}

func shellPathLine(dir string) string {
	return `export PATH="$PATH":` + shellSingleQuote(dir)
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
