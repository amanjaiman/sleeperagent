package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallExecutableCopiesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	target := filepath.Join(dir, "bin", "sleeperagent")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("sleeperagent binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	status, err := installExecutable(src, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if status != "installed" {
		t.Fatalf("status = %q, want installed", status)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sleeperagent binary" {
		t.Fatalf("installed content = %q", got)
	}

	status, err = installExecutable(src, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if status != "already installed" {
		t.Fatalf("status = %q, want already installed", status)
	}
}

func TestInstallExecutableRefusesDifferentExistingWithoutForce(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	target := filepath.Join(dir, "sleeperagent")
	if err := os.WriteFile(src, []byte("sleeperagent binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("different tool"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := installExecutable(src, target, false); err == nil {
		t.Fatal("expected refusal for different existing target")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "different tool" {
		t.Fatalf("target was overwritten without force: %q", got)
	}
}

func TestInstallExecutableForceOverwritesDifferentExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	target := filepath.Join(dir, "sleeperagent")
	if err := os.WriteFile(src, []byte("sleeperagent binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("different tool"), 0o755); err != nil {
		t.Fatal(err)
	}

	status, err := installExecutable(src, target, true)
	if err != nil {
		t.Fatal(err)
	}
	if status != "installed" {
		t.Fatalf("status = %q, want installed", status)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sleeperagent binary" {
		t.Fatalf("target content = %q", got)
	}
}

func TestInstallName(t *testing.T) {
	if got := installName("windows"); got != "sleeperagent.exe" {
		t.Fatalf("windows name = %q", got)
	}
	if got := installName("linux"); got != "sleeperagent" {
		t.Fatalf("unix name = %q", got)
	}
}

func TestPathContainsDir(t *testing.T) {
	// Hardcode the list separators rather than using os.PathListSeparator:
	// that constant is fixed to the build OS, but pathContainsDir takes an
	// explicit goos and must behave the same for any target regardless of
	// which OS this test binary itself was built for.
	if !pathContainsDir("/usr/bin:/home/me/.local/bin", "/home/me/.local/bin", "linux") {
		t.Fatal("expected linux PATH to contain dir")
	}
	if !pathContainsDir(`C:\Tools;C:\Users\me\bin`, `c:\users\me\bin`, "windows") {
		t.Fatal("expected windows PATH comparison to be case-insensitive")
	}
}

func TestEnsurePathInShellProfileAddsMacZProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	dir := filepath.Join(home, ".local", "bin")

	profile, updated, err := ensurePathInShellProfile(dir, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("expected profile to be updated")
	}
	wantProfile := filepath.Join(home, ".zprofile")
	if profile != wantProfile {
		t.Fatalf("profile = %q, want %q", profile, wantProfile)
	}
	got, err := os.ReadFile(wantProfile)
	if err != nil {
		t.Fatal(err)
	}
	wantLine := shellPathLine(dir)
	if !strings.Contains(string(got), wantLine) {
		t.Fatalf("profile does not contain %q:\n%s", wantLine, got)
	}

	_, updated, err = ensurePathInShellProfile(dir, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("expected second profile update to be idempotent")
	}
}

func TestShellProfileInHome(t *testing.T) {
	tests := []struct {
		shell, goos, want string
	}{
		{"/bin/zsh", "darwin", ".zprofile"},
		{"/usr/bin/zsh", "linux", ".zshrc"},
		{"/bin/bash", "darwin", ".bash_profile"},
		{"/bin/bash", "linux", ".bashrc"},
		{"/bin/sh", "linux", ".profile"},
		{"", "darwin", ".zprofile"},
		{"/usr/bin/fish", "linux", ""},
		{"/usr/bin/fish", "darwin", ""},
	}
	for _, tt := range tests {
		want := tt.want
		if want != "" {
			want = filepath.Join("home", want)
		}
		if got := shellProfileInHome("home", tt.shell, tt.goos); got != want {
			t.Errorf("shellProfileInHome(%q, %q) = %q, want %q", tt.shell, tt.goos, got, want)
		}
	}
}

func TestEnsurePathInShellProfileSkipsUnknownShell(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/usr/bin/fish")

	profile, updated, err := ensurePathInShellProfile(filepath.Join(home, "bin"), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "" || updated {
		t.Fatalf("expected no profile update for fish, got profile=%q updated=%v", profile, updated)
	}
}

func TestEnsurePathInShellProfileIgnoresCommentedMention(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")
	dir := filepath.Join(home, "bin")
	rc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(rc, []byte("# "+shellPathLine(dir)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	profile, updated, err := ensurePathInShellProfile(dir, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if profile != rc {
		t.Fatalf("profile = %q, want %q", profile, rc)
	}
	if !updated {
		t.Fatal("expected commented-out mention to still trigger an update")
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "\n"+shellPathLine(dir)) {
		t.Fatalf("profile missing uncommented PATH line:\n%s", got)
	}
}

func TestShellPathLineQuotesSpaces(t *testing.T) {
	got := shellPathLine("/Users/me/Library/Application Support/sleeperagent/bin")
	want := `export PATH="$PATH":'/Users/me/Library/Application Support/sleeperagent/bin'`
	if got != want {
		t.Fatalf("shellPathLine = %q, want %q", got, want)
	}
}
