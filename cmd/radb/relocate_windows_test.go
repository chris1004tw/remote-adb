//go:build windows

package main

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestExecutablePath_FallbackOnErrnoAccessDenied(t *testing.T) {
	origExe := osExecutable
	origEval := evalSymlinks
	defer func() {
		osExecutable = origExe
		evalSymlinks = origEval
	}()

	want := filepath.Join("C:", "temp", "radb.exe")
	osExecutable = func() (string, error) { return want, nil }
	evalSymlinks = func(string) (string, error) {
		return "", syscall.ERROR_ACCESS_DENIED
	}

	got, err := executablePath()
	if err != nil {
		t.Fatalf("executablePath returned error: %v", err)
	}
	if got != want {
		t.Fatalf("executablePath = %q, want %q", got, want)
	}
}
