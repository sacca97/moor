package session

import (
	"os"
	"testing"
)

func useTempRoot(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

// Session creation, listing, stale cleanup and ID/name allocation are covered
// end to end in cmd/moor; this file holds the pure logic worth a table.

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"work", "ghg", "a.b_c-d", "x1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "3", "has space", "a/b", "-x"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) succeeded", bad)
		}
	}
}

func TestAutoName(t *testing.T) {
	cases := map[string]string{
		"":                              "shell",
		"codex":                         "codex",
		"codex --full-auto":             "codex",
		"python server.py":              "python",
		"cd ~/ghg && codex":             "codex",
		"cd ~/project; export X=1; vim": "vim",
		"/usr/local/bin/htop":           "htop",
		"FOO=bar BAZ=1 make -j8":        "make",
		"env -i TERM=xterm top":         "top",
		"sudo -E nvim /etc/hosts":       "nvim",
		"tail -f log | grep err":        "tail",
		"cd /tmp":                       "shell",
		"sleep 2; echo hello":           "echo",
		"make || echo failed":           "echo",
		"npm run dev > out.log 2>&1 &":  "npm",
		"'my prog' arg":                 "my-prog",
		"(cd sub && cargo run)":         "cargo",
		"exec ./run.sh":                 "run.sh",
		"time go test ./...":            "go",
		"> out.log python x.py":         "python",
		"42":                            "cmd-42",
		"émoji✓ok":                      "moji-ok",
		"$$$":                           "shell",
	}
	for in, want := range cases {
		if got := AutoName(in); got != want {
			t.Errorf("AutoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoveChecksOwner(t *testing.T) {
	useTempRoot(t)
	EnsureRoot()
	os.MkdirAll(Dir(0), 0o700)
	WriteMeta(Dir(0), Meta{ID: 0, PID: 1234})
	if err := Remove(0, 4321); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Dir(0)); err != nil {
		t.Fatal("Remove deleted a directory owned by another server")
	}
	if err := Remove(0, 1234); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Dir(0)); !os.IsNotExist(err) {
		t.Fatal("Remove did not delete its own directory")
	}
}
