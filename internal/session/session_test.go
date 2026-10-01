package session

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sacca/moor/internal/protocol"
)

func useTempRoot(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

func TestLowestFree(t *testing.T) {
	cases := []struct {
		used []int
		want int
	}{
		{nil, 0},
		{[]int{0}, 1},
		{[]int{0, 1, 3}, 2},
		{[]int{1, 2}, 0},
		{[]int{3, 0, 2, 1}, 4},
	}
	for _, c := range cases {
		if got := LowestFree(c.used); got != c.want {
			t.Errorf("LowestFree(%v) = %d, want %d", c.used, got, c.want)
		}
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"codex":      "codex",
		"my session": "my-session",
		"a//b::c":    "a-b-c",
		"--x--":      "x",
		".hidden":    "hidden",
		"émoji✓ok":   "moji-ok",
		"v1.2_rc-3":  "v1.2_rc-3",
		"":           "",
		"$$$":        "",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

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
	}
	for in, want := range cases {
		if got := AutoName(in); got != want {
			t.Errorf("AutoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsDefaultName(t *testing.T) {
	for name, want := range map[string]bool{
		"shell": true, "shell-1": true, "shell-12": true,
		"shell-0": false, "shells": false, "shell-x": false, "work": false, "codex": false,
	} {
		if got := IsDefaultName(name); got != want {
			t.Errorf("IsDefaultName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestUniqueName(t *testing.T) {
	taken := map[string]bool{"shell": true, "shell-1": true, "codex": true}
	if got := UniqueName("shell", taken); got != "shell-2" {
		t.Errorf("got %q", got)
	}
	if got := UniqueName("vim", taken); got != "vim" {
		t.Errorf("got %q", got)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := Meta{
		ID: 3, Name: "codex", PID: 100, ShellPID: 101,
		CreatedAt: time.Date(2026, 10, 1, 20, 0, 0, 0, time.FixedZone("", 7200)),
		CWD:       "/home/user/project", Command: "codex", Attached: true,
	}
	if err := WriteMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("created_at %v != %v", got.CreatedAt, m.CreatedAt)
	}
	got.CreatedAt = m.CreatedAt
	if got != m {
		t.Fatalf("got %+v, want %+v", got, m)
	}
	fi, err := os.Stat(filepath.Join(dir, metaFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("metadata mode %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestRootPermissions(t *testing.T) {
	useTempRoot(t)
	root, err := EnsureRoot()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("root mode %v, want 0700", fi.Mode().Perm())
	}
}

// fakeServer answers pings on the socket of session id like a real server.
func fakeServer(t *testing.T, id int, name string, attached bool) {
	t.Helper()
	if err := os.MkdirAll(Dir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(Dir(id), Meta{ID: id, Name: name, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", SocketPath(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if typ, _, err := protocol.ReadFrame(conn); err == nil && typ == protocol.MsgPing {
					b := byte(0)
					if attached {
						b = 1
					}
					protocol.WriteFrame(conn, protocol.MsgPong, []byte{b})
				}
			}()
		}
	}()
}

func TestStaleSessionDetection(t *testing.T) {
	useTempRoot(t)
	if _, err := EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	fakeServer(t, 0, "alive", false)
	fakeServer(t, 3, "busy", true)

	// Session 1: metadata but no socket (server crashed during startup).
	os.MkdirAll(Dir(1), 0o700)
	WriteMeta(Dir(1), Meta{ID: 1, Name: "dead", PID: 999999})
	// Session 2: a socket file nobody listens on (server crashed).
	os.MkdirAll(Dir(2), 0o700)
	WriteMeta(Dir(2), Meta{ID: 2, Name: "crashed"})
	ln, err := net.Listen("unix", SocketPath(2))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()

	sessions, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Name != "alive" || sessions[1].Name != "busy" {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
	if sessions[0].Attached || !sessions[1].Attached {
		t.Fatalf("wrong attach status: %+v", sessions)
	}
	for _, id := range []int{1, 2} {
		if _, err := os.Stat(Dir(id)); !os.IsNotExist(err) {
			t.Errorf("stale session %d was not removed", id)
		}
	}

	// IDs 1 and 2 are free again; lowest is 1.
	_, used, err := scanLocked()
	if err != nil {
		t.Fatal(err)
	}
	if got := LowestFree(used); got != 1 {
		t.Fatalf("next ID = %d, want 1", got)
	}

	if s, err := Resolve("busy"); err != nil || s.ID != 3 {
		t.Fatalf("Resolve(busy) = %+v, %v", s, err)
	}
	if s, err := Resolve("0"); err != nil || s.Name != "alive" {
		t.Fatalf("Resolve(0) = %+v, %v", s, err)
	}
	if _, err := Resolve("nope"); err == nil {
		t.Fatal("Resolve(nope) succeeded")
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
