package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// stampFile, inside a venv, holds the digest of the requirement files it was
// installed from, so an unchanged set is never installed twice.
const stampFile = ".brainiac-requirements.sha256"

// VenvPython is the interpreter inside a venv directory.
func VenvPython(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "Scripts", "python.exe")
	}
	return filepath.Join(dir, "bin", "python")
}

// requirementsDigest is the sha256 of the requirement files' names and contents, in order.
func requirementsDigest(requirements []string) (string, error) {
	h := sha256.New()
	for _, path := range requirements {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("worker: read %s: %w", path, err)
		}
		fmt.Fprintf(h, "%s\n%d\n", filepath.Base(path), len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// EnsureVenv makes dir a venv of basePython with the packages of the
// requirement files installed, and returns its interpreter. It does nothing
// when the venv exists and was installed from the same files (a stamp holds
// their digest), so it is safe to call on every start.
func EnsureVenv(ctx context.Context, basePython, dir string, requirements []string, logf func(string, ...any)) (string, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	digest, err := requirementsDigest(requirements)
	if err != nil {
		return "", err
	}
	python := VenvPython(dir)
	stamp := filepath.Join(dir, stampFile)
	if _, err := os.Stat(python); err == nil {
		if old, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(old)) == digest {
			return python, nil
		}
	} else {
		logf("creating a Python environment in %s", dir)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", fmt.Errorf("worker: %w", err)
		}
		if err := run(ctx, logf, basePython, "-m", "venv", dir); err != nil {
			return "", fmt.Errorf("worker: create venv: %w", err)
		}
	}
	if len(requirements) > 0 {
		args := []string{"-m", "pip", "install", "--disable-pip-version-check", "--no-input"}
		for _, r := range requirements {
			args = append(args, "-r", r)
		}
		logf("installing %s", strings.Join(baseNames(requirements), ", "))
		if err := run(ctx, logf, python, args...); err != nil {
			return "", fmt.Errorf("worker: pip install: %w", err)
		}
	}
	if err := os.WriteFile(stamp, []byte(digest+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("worker: write %s: %w", stamp, err)
	}
	return python, nil
}

// baseNames is each path's last element.
func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// run executes a command, sending its output to logf line by line, and keeps
// the last lines for the error when it fails.
func run(ctx context.Context, logf func(string, ...any), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	last := &tail{max: 2048}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			if line == "" {
				continue
			}
			logf("  %s", line)
			_, _ = last.Write([]byte(line + "\n"))
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	err := cmd.Run()
	_ = pw.Close()
	<-done
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", filepath.Base(name), err, strings.TrimSpace(last.String()))
	}
	return nil
}
