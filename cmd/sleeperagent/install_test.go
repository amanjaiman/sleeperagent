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

func TestShellPathLineQuotesSpaces(t *testing.T) {
	got := shellPathLine("/Users/me/Library/Application Support/sleeperagent/bin")
	want := `export PATH="$PATH":'/Users/me/Library/Application Support/sleeperagent/bin'`
	if got != want {
		t.Fatalf("shellPathLine = %q, want %q", got, want)
	}
}
