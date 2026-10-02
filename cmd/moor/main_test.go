package main

import (
	"bytes"
	"io"
	"os"
	"testing"
)

func TestRunVersionUsesMainVersion(t *testing.T) {
	oldVersion := version
	oldStdout := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdout pipe: %v", err)
	}

	version = "9.9.9-test"
	os.Stdout = w

	code := run([]string{"--version"})

	_ = w.Close()
	os.Stdout = oldStdout
	version = oldVersion

	if code != 0 {
		t.Fatalf("run returned %d, want 0", code)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("reading stdout: %v", err)
	}
	_ = r.Close()

	if got, want := buf.String(), "moor 9.9.9-test\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}
