package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func installCmd(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory to install agentkeeper into")
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
	fmt.Printf("agentkeeper install: %s: %s\n", status, target)
	if pathContainsDir(os.Getenv("PATH"), targetDir, runtime.GOOS) {
		fmt.Println("Open a new shell, then run: agentkeeper version")
		return nil
	}
	fmt.Printf("agentkeeper install: %s is not on PATH yet.\n", targetDir)
	fmt.Println(pathRemediation(targetDir, runtime.GOOS))
	fmt.Println("Open a new shell after updating PATH, then run: agentkeeper version")
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
		return "agentkeeper.exe"
	}
	return "agentkeeper"
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

	tmp := target + ".tmp"
	if err := copyFile(src, tmp, srcInfo.Mode()|0o755); err != nil {
		return "", err
	}
	if force {
		_ = os.Remove(target)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
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

func pathContainsDir(pathValue, dir, goos string) bool {
	want := cleanPathForCompare(dir, goos)
	for _, entry := range filepath.SplitList(pathValue) {
		if cleanPathForCompare(entry, goos) == want {
			return true
		}
	}
	return false
}

func cleanPathForCompare(path, goos string) string {
	cleaned := filepath.Clean(path)
	if goos == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func pathRemediation(dir, goos string) string {
	if goos == "windows" {
		return fmt.Sprintf("Add it for future shells with:\n  setx PATH \"%%PATH%%;%s\"\nOr add it in Settings > System > About > Advanced system settings > Environment Variables.", dir)
	}
	return fmt.Sprintf("Add it for future shells with:\n  export PATH=\"$PATH:%s\"\nPut that line in your shell profile if you want it to persist.", dir)
}
