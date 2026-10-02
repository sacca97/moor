package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "moor-bin-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "moor")
	out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "building moor: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// env is an isolated runtime directory and environment for one test.
type env struct {
	t    *testing.T
	root string
	vars []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "XDG_RUNTIME_DIR", "SHELL", "HOME", "ENV", "PS1", "TERM", "MOOR_SESSION", "PROMPT_COMMAND", "BASH_ENV":
			continue
		}
		e.vars = append(e.vars, kv)
	}
	e.vars = append(e.vars,
		"XDG_RUNTIME_DIR="+root,
		"SHELL=/bin/sh",
		"HOME="+root,
		"PS1=dsh$ ",
		"TERM=xterm-256color",
	)
	t.Cleanup(e.killAll)
	return e
}

// killAll terminates any session servers the test left behind.
func (e *env) killAll() {
	dirs, _ := filepath.Glob(filepath.Join(e.root, "moor", "[0-9]*"))
	for _, d := range dirs {
		data, err := os.ReadFile(filepath.Join(d, "session.json"))
		if err != nil {
			continue
		}
		var m struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(data, &m) == nil && m.PID > 0 {
			syscall.Kill(m.PID, syscall.SIGKILL)
		}
	}
}

// run runs moor without a terminal and returns its combined output.
func (e *env) run(args ...string) (string, error) {
	cmd := exec.Command(binary, args...)
	cmd.Env = e.vars
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) mustRun(args ...string) string {
	e.t.Helper()
	out, err := e.run(args...)
	if err != nil {
		e.t.Fatalf("moor %v: %v\n%s", args, err, out)
	}
	return out
}

func (e *env) ps() string {
	e.t.Helper()
	return e.mustRun("ps")
}

// termClient is moor running on a pseudo-terminal, standing in for the
// user's terminal emulator.
type termClient struct {
	t    *testing.T
	ptmx *os.File
	cmd  *exec.Cmd

	mu     sync.Mutex
	output bytes.Buffer
	exited chan struct{}
}

func (e *env) attachTerm(rows, cols uint16, args ...string) *termClient {
	e.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = e.vars
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		e.t.Fatal(err)
	}
	c := &termClient{t: e.t, ptmx: ptmx, cmd: cmd, exited: make(chan struct{})}
	go c.readLoop()
	go func() {
		cmd.Wait()
		close(c.exited)
	}()
	e.t.Cleanup(func() {
		cmd.Process.Kill()
		ptmx.Close()
	})
	return c
}

// terminalReplies answers the status and cursor-position queries in out, in
// the order they appear, like a real terminal.
func terminalReplies(out []byte) []byte {
	var replies []byte
	for i := 0; i+3 < len(out); i++ {
		switch string(out[i : i+4]) {
		case "\x1b[5n":
			replies = append(replies, "\x1b[0n"...)
		case "\x1b[6n":
			replies = append(replies, "\x1b[12;1R"...)
		}
	}
	return replies
}

func (c *termClient) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := c.ptmx.Read(buf)
		if n > 0 {
			c.mu.Lock()
			c.output.Write(buf[:n])
			c.mu.Unlock()
			c.ptmx.Write(terminalReplies(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

func (c *termClient) send(s string) {
	c.t.Helper()
	if _, err := c.ptmx.Write([]byte(s)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *termClient) expect(substr string) {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		ok := strings.Contains(c.output.String(), substr)
		c.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t.Fatalf("timed out waiting for %q; output so far:\n%q", substr, c.output.String())
}

func (c *termClient) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.output.String(), substr)
}

func (c *termClient) waitExit() {
	c.t.Helper()
	select {
	case <-c.exited:
	case <-time.After(10 * time.Second):
		c.t.Fatal("client did not exit")
	}
}

func (c *termClient) detach() {
	c.t.Helper()
	c.send("\x1c\x1c") // Ctrl-\ Ctrl-\
	c.expect("[moor: detached from session")
	c.waitExit()
}

func psLine(ps string, id int) string {
	for _, l := range strings.Split(ps, "\n") {
		f := strings.Fields(l)
		if len(f) > 0 && f[0] == fmt.Sprint(id) {
			return strings.Join(f, " ")
		}
	}
	return ""
}

func TestVersion(t *testing.T) {
	e := newEnv(t)
	for _, arg := range []string{"--version", "version"} {
		if out := e.mustRun(arg); !strings.HasPrefix(out, "moor ") {
			t.Fatalf("moor %s printed %q", arg, out)
		}
	}
}

func TestRunDetachedThenAttachReplaysScrollback(t *testing.T) {
	e := newEnv(t)
	out := e.mustRun("run", "sleep 1; echo hel''lo")
	if !strings.Contains(out, "started session 0 (echo)") {
		t.Fatalf("unexpected run output %q", out)
	}
	if l := psLine(e.ps(), 0); !strings.HasPrefix(l, "0 echo detached ") {
		t.Fatalf("ps line %q", l)
	}
	time.Sleep(1500 * time.Millisecond)

	c := e.attachTerm(24, 80, "-a", "0")
	c.expect("hello")
	if l := psLine(e.ps(), 0); !strings.HasPrefix(l, "0 echo attached ") {
		t.Fatalf("ps line while attached %q", l)
	}
	c.detach()
	if l := psLine(e.ps(), 0); !strings.HasPrefix(l, "0 echo detached ") {
		t.Fatalf("ps line after detach %q", l)
	}
}

func TestSessionLifecycle(t *testing.T) {
	e := newEnv(t)
	c := e.attachTerm(24, 80, "-n", "work")
	c.expect("dsh$ ")
	for path, mode := range map[string]os.FileMode{"moor": 0o700, "moor/0/session.json": 0o600} {
		if fi, err := os.Stat(filepath.Join(e.root, path)); err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v, %v; want mode %v", path, fi, err, mode)
		}
	}
	c.send("echo marker-$MOOR_SESSION\r")
	c.expect("marker-0")

	// Output produced while detached must survive.
	c.send("sleep 1; echo after-$((1+1))-detach\r")
	c.detach()
	if l := psLine(e.ps(), 0); !strings.HasPrefix(l, "0 work detached ") {
		t.Fatalf("ps line %q", l)
	}
	time.Sleep(1500 * time.Millisecond)

	c = e.attachTerm(24, 80, "attach", "work")
	c.expect("after-2-detach")

	// Resize follows the client terminal.
	if err := pty.Setsize(c.ptmx, &pty.Winsize{Rows: 33, Cols: 101}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	c.send("stty size\r")
	c.expect("33 101")

	// Esc Esc, arrow keys and a single Ctrl-\ reach the shell; none of
	// them detaches.
	c.send("\x1b\x1b")
	c.send("\x1b[A\x1b[B")
	c.send("\x1c")
	time.Sleep(600 * time.Millisecond)
	c.send("\x15echo still-$((2+3))\r") // ^U clears whatever the keys did
	c.expect("still-5")

	c.send("exit\r")
	c.expect("[moor: session 0 (work) exited]")
	c.waitExit()
	if l := psLine(e.ps(), 0); l != "" {
		t.Fatalf("session still listed after exit: %q", l)
	}
	if _, err := os.Stat(filepath.Join(e.root, "moor", "0")); !os.IsNotExist(err) {
		t.Fatal("session directory not removed after exit")
	}
}

func TestNewestClientWritesOthersWatch(t *testing.T) {
	e := newEnv(t)
	a := e.attachTerm(24, 80, "-n", "work")
	a.expect("dsh$ ")

	// A second client takes over; the first becomes read-only but keeps
	// seeing the output.
	b := e.attachTerm(30, 100, "attach", "work")
	b.expect("dsh$ ")
	a.expect("moor: read-only")
	if l := psLine(e.ps(), 0); !strings.HasPrefix(l, "0 work attached (2) ") {
		t.Fatalf("ps line %q", l)
	}
	b.send("echo from-b-$((1+1))\r")
	b.expect("from-b-2")
	a.expect("from-b-2")

	// The read-only client's keystrokes go nowhere, and the PTY has the
	// writer's size.
	a.send("echo from-a-$((2+2))\r")
	time.Sleep(300 * time.Millisecond)
	b.send("stty size\r")
	b.expect("30 100")
	if a.contains("from-a-4") || b.contains("from-a-4") {
		t.Fatal("input from the read-only client reached the session")
	}

	// When the writer detaches, the first client gets control and its size
	// back.
	b.detach()
	a.send("echo back-$((4+4))\r")
	a.expect("back-8")
	a.send("stty size\r")
	a.expect("24 80")
	if l := psLine(e.ps(), 0); !strings.HasPrefix(l, "0 work attached ") || strings.Contains(l, "(") {
		t.Fatalf("ps line %q", l)
	}
	a.detach()
}

func TestReadOnlyAttach(t *testing.T) {
	e := newEnv(t)
	a := e.attachTerm(24, 80, "-n", "work")
	a.expect("dsh$ ")

	w := e.attachTerm(24, 80, "attach", "-r", "work")
	w.expect("dsh$ ")
	w.send("echo from-w-$((5+5))\r")
	a.send("echo from-a-$((6+6))\r")
	a.expect("from-a-12")
	w.expect("from-a-12")
	if a.contains("from-w-10") || w.contains("from-w-10") {
		t.Fatal("input from the read-only client reached the session")
	}
	// The read-only client can still detach, and the writer is unaffected.
	w.detach()
	a.send("echo still-$((7+7))\r")
	a.expect("still-14")
	a.send("exit\r")
	a.waitExit()
}

// Ctrl-\ twice detaches however the terminal encodes the key.
func TestDetachKeyEncodings(t *testing.T) {
	e := newEnv(t)
	for name, keys := range map[string]string{
		"legacy":          "\x1c\x1c",
		"kitty":           "\x1b[92;5u\x1b[92;5:1u",
		"modifyOtherKeys": "\x1b[27;5;92~\x1b[27;5;92~",
	} {
		c := e.attachTerm(24, 80, "-n", name)
		c.expect("dsh$ ")
		c.send(keys)
		c.expect("[moor: detached")
		c.waitExit()
	}
	if got := strings.Count(e.ps(), "detached"); got != 3 {
		t.Fatalf("want 3 live detached sessions:\n%s", e.ps())
	}
}

// A program that switches to the alternate screen, with the sequence split
// across writes, is left again on detach so the terminal is usable.
func TestDetachLeavesAlternateScreen(t *testing.T) {
	e := newEnv(t)
	c := e.attachTerm(24, 80)
	c.expect("dsh$ ")
	c.send("printf '\\033[?10'; sleep 0.2; printf '49h'; echo done-$((1+1))\r")
	c.expect("done-2")
	c.detach()
	c.expect("\x1b[?1049l")
}

// Terminal replies to queries in the replayed output must not reach the shell.
func TestReplayedQueriesAreNotAnswered(t *testing.T) {
	e := newEnv(t)
	e.mustRun("run", "printf '\\033[6n'")
	time.Sleep(500 * time.Millisecond)
	c := e.attachTerm(24, 80, "-a", "0")
	c.expect("dsh$ ")
	c.send("echo after-$((1+1))\r")
	c.expect("after-2")
	if c.contains("12;1R") {
		t.Fatal("the terminal's reply to a replayed query reached the shell")
	}
}

// A frozen read-only watcher must not hold up the session for long.
func TestFrozenWatcherDoesNotStallSession(t *testing.T) {
	e := newEnv(t)
	a := e.attachTerm(24, 80, "-n", "work")
	a.expect("dsh$ ")
	w := e.attachTerm(24, 80, "attach", "-r", "work")
	w.expect("dsh$ ")
	w.cmd.Process.Signal(syscall.SIGSTOP)
	defer w.cmd.Process.Signal(syscall.SIGCONT)

	start := time.Now()
	a.send("head -c 20000000 /dev/zero | tr '\\0' x; echo flood-$((1+1))\r")
	a.expect("flood-2")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a frozen watcher stalled the session for %v", d)
	}
}

// A client that is slow to take its scrollback replay must not hold up the
// session while it does.
func TestSlowReplayDoesNotStallSession(t *testing.T) {
	e := newEnv(t)
	a := e.attachTerm(24, 80, "-n", "work")
	a.expect("dsh$ ")
	a.send("head -c 12000000 /dev/zero | tr '\\0' x; echo flooded-$((1+1))\r")
	a.expect("flooded-2")

	w := e.attachTerm(24, 80, "attach", "-r", "work")
	w.expect("\x1b[3J") // the replay has begun
	w.cmd.Process.Signal(syscall.SIGSTOP)
	defer w.cmd.Process.Signal(syscall.SIGCONT)

	start := time.Now()
	a.send("echo alive-$((3+4))\r")
	a.expect("alive-7")
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("a replaying client stalled the session for %v", d)
	}
}

func TestStartInjectsCommandAndKeepsShell(t *testing.T) {
	e := newEnv(t)
	c := e.attachTerm(24, 80, "start", "sh", "-c", "echo injected-$((40+2))")
	c.expect("injected-42")
	c.expect("dsh$ ")
	c.send("echo shell-$((1+2))\r")
	c.expect("shell-3")
	if l := psLine(e.ps(), 0); !strings.HasPrefix(l, "0 sh attached ") {
		t.Fatalf("ps line %q", l)
	}
	c.send("exit\r")
	c.waitExit()
}

func TestNamesAndIDs(t *testing.T) {
	e := newEnv(t)
	e.mustRun("-n", "project", "run", "cd / && sleep 60")
	if _, err := e.run("-n", "project", "run"); err == nil {
		t.Fatal("duplicate explicit name accepted")
	}
	e.mustRun("run")
	e.mustRun("run")
	ps := e.ps()
	for id, want := range map[int]string{0: "0 project ", 1: "1 shell ", 2: "2 shell-1 "} {
		if l := psLine(ps, id); !strings.HasPrefix(l, want) {
			t.Fatalf("session %d: ps line %q, want prefix %q\n%s", id, l, want, ps)
		}
	}

	if out := e.mustRun("kill", "1"); !strings.Contains(out, "killed session 1") {
		t.Fatalf("kill output %q", out)
	}
	if out := e.mustRun("run", "vim"); !strings.Contains(out, "started session 1 (vim)") {
		t.Fatalf("lowest free ID not reused: %q", out)
	}
	e.mustRun("kill", "project")
	if l := psLine(e.ps(), 0); l != "" {
		t.Fatalf("killed session still listed: %q", l)
	}
}

func TestConcurrentCreation(t *testing.T) {
	e := newEnv(t)
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := e.run("run"); err != nil {
				errs <- fmt.Errorf("%v: %s", err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ps := e.ps()
	for id := 0; id < n; id++ {
		if psLine(ps, id) == "" {
			t.Fatalf("session %d missing:\n%s", id, ps)
		}
	}
}

// Servers that do not answer must cost one probe timeout in total, not one each.
func TestPsWithUnresponsiveServers(t *testing.T) {
	e := newEnv(t)
	const n = 4
	for i := 0; i < n; i++ {
		e.mustRun("run")
	}
	for i := 0; i < n; i++ {
		data, err := os.ReadFile(filepath.Join(e.root, "moor", fmt.Sprint(i), "session.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			PID int `json:"pid"`
		}
		json.Unmarshal(data, &m)
		syscall.Kill(m.PID, syscall.SIGSTOP)
		defer syscall.Kill(m.PID, syscall.SIGKILL)
	}
	start := time.Now()
	e.ps()
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("ps took %v with %d unresponsive servers", d, n)
	}
}

func TestStaleSessionCleanup(t *testing.T) {
	e := newEnv(t)
	e.mustRun("run")
	data, err := os.ReadFile(filepath.Join(e.root, "moor", "0", "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		PID int `json:"pid"`
	}
	json.Unmarshal(data, &m)
	syscall.Kill(m.PID, syscall.SIGKILL) // simulate a server crash
	time.Sleep(100 * time.Millisecond)

	if l := psLine(e.ps(), 0); l != "" {
		t.Fatalf("crashed session still listed: %q", l)
	}
	if _, err := os.Stat(filepath.Join(e.root, "moor", "0")); !os.IsNotExist(err) {
		t.Fatal("stale directory not removed")
	}
}

// setVar overrides one environment variable for moor and its shells.
func (e *env) setVar(key, value string) {
	vars := e.vars[:0]
	for _, kv := range e.vars {
		if !strings.HasPrefix(kv, key+"=") {
			vars = append(vars, kv)
		}
	}
	e.vars = append(vars, key+"="+value)
}

func TestPromptMarker(t *testing.T) {
	const mark = "\x1b[2m[moor:work]\x1b[22m "
	for _, shell := range []string{"/bin/bash", "/bin/zsh"} {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			if _, err := os.Stat(shell); err != nil {
				t.Skip(shell + " not installed")
			}
			e := newEnv(t)
			e.setVar("SHELL", shell)
			// The user's own startup file must still be loaded, and its
			// history setup honored.
			rc := "export USER_RC=loaded-$((1+1))\nHISTFILE=$HOME/hist\n"
			if strings.HasSuffix(shell, "zsh") {
				rc += "PROMPT='dsh$ '\nHISTSIZE=100 SAVEHIST=100\nsetopt INC_APPEND_HISTORY\n"
			} else {
				rc += "PS1='dsh$ '\nPROMPT_COMMAND='history -a'\n"
			}
			for _, f := range []string{".bashrc", ".zshrc"} {
				os.WriteFile(filepath.Join(e.root, f), []byte(rc), 0o600)
			}

			c := e.attachTerm(24, 80, "-n", "work")
			c.expect(mark + "dsh$ ")
			c.send("echo $USER_RC $ZDOTDIR.\r")
			c.expect("loaded-2 .") // ZDOTDIR restored to the user's (unset)
			c.send("exit\r")
			c.waitExit()
			if hist, _ := os.ReadFile(filepath.Join(e.root, "hist")); !strings.Contains(string(hist), "echo $USER_RC") {
				t.Fatalf("history not saved: %q", hist)
			}
		})
	}
}

func TestPromptMarkerDisabled(t *testing.T) {
	e := newEnv(t)
	e.setVar("SHELL", "/bin/bash")
	e.setVar("MOOR_PROMPT", "0")
	os.WriteFile(filepath.Join(e.root, ".bashrc"), []byte("PS1='dsh$ '\n"), 0o600)
	c := e.attachTerm(24, 80, "-n", "work")
	c.expect("dsh$ ")
	c.send("echo $MOOR_SESSION_NAME-$((1+1))\r")
	c.expect("work-2")
	c.mu.Lock()
	out := c.output.String()
	c.mu.Unlock()
	if strings.Contains(out, "[work]") {
		t.Fatalf("marker shown although disabled: %q", out)
	}
	c.send("exit\r")
	c.waitExit()
}

func TestAttachClearsScreen(t *testing.T) {
	e := newEnv(t)
	c := e.attachTerm(24, 80)
	c.expect("\x1b[H\x1b[2J\x1b[3J")
	c.expect("dsh$ ")
	c.send("exit\r")
	c.waitExit()
}

func TestPromptMarkerUnnamed(t *testing.T) {
	e := newEnv(t)
	e.setVar("SHELL", "/bin/bash")
	os.WriteFile(filepath.Join(e.root, ".bashrc"), []byte("PS1='dsh$ '\n"), 0o600)
	c := e.attachTerm(24, 80)
	c.expect("\x1b[2m[moor]\x1b[22m dsh$ ")
	c.send("exit\r")
	c.expect("[moor: session 0 exited]")
	c.waitExit()
}
