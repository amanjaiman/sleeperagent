package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallExecutableCopiesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	target := filepath.Join(dir, "bin", "agentkeeper")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("agentkeeper binary"), 0o755); err != nil {
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
	if string(got) != "agentkeeper binary" {
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
	target := filepath.Join(dir, "agentkeeper")
	if err := os.WriteFile(src, []byte("agentkeeper binary"), 0o755); err != nil {
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
	target := filepath.Join(dir, "agentkeeper")
	if err := os.WriteFile(src, []byte("agentkeeper binary"), 0o755); err != nil {
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
	if string(got) != "agentkeeper binary" {
		t.Fatalf("target content = %q", got)
	}
}

func TestInstallName(t *testing.T) {
	if got := installName("windows"); got != "agentkeeper.exe" {
		t.Fatalf("windows name = %q", got)
	}
	if got := installName("linux"); got != "agentkeeper" {
		t.Fatalf("unix name = %q", got)
	}
}

func TestPathContainsDir(t *testing.T) {
	if !pathContainsDir("/usr/bin"+string(os.PathListSeparator)+"/home/me/.local/bin", "/home/me/.local/bin", "linux") {
		t.Fatal("expected linux PATH to contain dir")
	}
	if !pathContainsDir(`C:\Tools;C:\Users\me\bin`, `c:\users\me\bin`, "windows") {
		t.Fatal("expected windows PATH comparison to be case-insensitive")
	}
}
