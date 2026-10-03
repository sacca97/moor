package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "moor-bin-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "moor")
	out, err := exec.Command("go", "build", "-ldflags", "-X main.version=0.1.0", "-o", binary, ".").CombinedOutput()
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
	cwd  string // directory moor runs in; empty means the test's own
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
		"MOOR_NO_UPDATE_CHECK=1", // the update test turns it back on
	)
	t.Cleanup(e.killAll)
	return e
}

// killAll terminates every session the test left behind, shells included,
// and waits until they are gone: a shell that outlives its server would
// otherwise write its history into the test's temporary directory while it is
// being removed.
func (e *env) killAll() {
	type proc struct{ server, shell int }
	var procs []proc
	dirs, _ := filepath.Glob(filepath.Join(e.root, "moor", "[0-9]*"))
	for _, d := range dirs {
		data, err := os.ReadFile(filepath.Join(d, "session.json"))
		if err != nil {
			continue
		}
		var m struct {
			PID      int `json:"pid"`
			ShellPID int `json:"shell_pid"`
		}
		if json.Unmarshal(data, &m) == nil {
			procs = append(procs, proc{m.PID, m.ShellPID})
		}
	}
	alive := func(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }
	wait := func(d time.Duration) bool {
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			done := true
			for _, p := range procs {
				done = done && !alive(p.server) && !alive(p.shell)
			}
			if done {
				return true
			}
		}
		return false
	}
	// Ask first, so servers hang up their shells and remove their directories.
	for _, p := range procs {
		if p.server > 0 {
			syscall.Kill(p.server, syscall.SIGCONT) // it may be stopped by a test
			syscall.Kill(p.server, syscall.SIGTERM)
		}
	}
	if wait(3 * time.Second) {
		return
	}
	for _, p := range procs {
		syscall.Kill(p.server, syscall.SIGKILL)
		if p.shell > 0 {
			syscall.Kill(-p.shell, syscall.SIGKILL)
			syscall.Kill(p.shell, syscall.SIGKILL)
		}
	}
	wait(2 * time.Second)
}

// run runs moor without a terminal and returns its combined output.
func (e *env) run(args ...string) (string, error) {
	cmd := exec.Command(binary, args...)
	cmd.Env = e.vars
	cmd.Dir = e.cwd
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
	return e.startTerm(true, rows, cols, args...)
}

// startTerm runs moor on a pty. With drain false nothing reads the pty, like
// a terminal that has stopped consuming output.
func (e *env) startTerm(drain bool, rows, cols uint16, args ...string) *termClient {
	e.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = e.vars
	cmd.Dir = e.cwd
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		e.t.Fatal(err)
	}
	c := &termClient{t: e.t, ptmx: ptmx, cmd: cmd, exited: make(chan struct{})}
	if drain {
		go c.readLoop()
	}
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

// cooked reports whether the terminal is in its normal line-editing state
// (canonical mode with echo), as opposed to the raw mode moor runs in.
func (c *termClient) cooked() bool {
	raw, err := c.ptmx.SyscallConn()
	if err != nil {
		c.t.Fatal(err)
	}
	var t *unix.Termios
	raw.Control(func(fd uintptr) { t, err = unix.IoctlGetTermios(int(fd), getTermios) })
	if err != nil {
		c.t.Fatal(err)
	}
	return t.Lflag&unix.ICANON != 0 && t.Lflag&unix.ECHO != 0
}

func (c *termClient) count(substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Count(c.output.String(), substr)
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
		"tmux":            "\x02d",
		"tmuxkitty":       "\x1b[98;5u\x1b[100u",
	} {
		c := e.attachTerm(24, 80, "-n", name)
		c.expect("dsh$ ")
		c.send(keys)
		c.expect("[moor: detached")
		c.waitExit()
	}
	if got := strings.Count(e.ps(), "detached"); got != 5 {
		t.Fatalf("want 5 live detached sessions:\n%s", e.ps())
	}
}

// Like tmux's prefix, Ctrl-b waits for the next key however long it takes
// (a touch keyboard is slow), and is otherwise passed through in order.
func TestCtrlBWaitsForNextKey(t *testing.T) {
	e := newEnv(t)
	c := e.attachTerm(24, 80, "-n", "tmuxlike")
	c.expect("dsh$ ")

	// A key other than d: the Ctrl-b (backward-char in readline) is released
	// before it, so X lands between b and c.
	c.send("echo abc\x02")
	time.Sleep(1200 * time.Millisecond)
	c.send("X\r")
	c.expect("abXc")

	// Ctrl-b Ctrl-b sends exactly one Ctrl-b, one step back.
	c.send("echo pqr\x02\x02X\r")
	c.expect("pqXr")

	// d detaches even after a long pause.
	c.send("\x02")
	time.Sleep(1500 * time.Millisecond)
	c.send("d")
	c.expect("[moor: detached")
	c.waitExit()
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

// However an attachment ends, the terminal must be left usable: cooked mode
// with echo, cursor visible.
func TestTerminalRestoredOnEveryExit(t *testing.T) {
	e := newEnv(t)
	killServer := func() { syscall.Kill(e.serverPID(0), syscall.SIGKILL) }
	for name, s := range map[string]struct {
		end    func(c *termClient)
		expect string
	}{
		"detach":         {func(c *termClient) { c.send("\x1c\x1c") }, "detached from"},
		"shell exit":     {func(c *termClient) { c.send("exit\r") }, "exited"},
		"moor kill":      {func(c *termClient) { e.mustRun("kill", "0") }, "exited"},
		"server dies":    {func(c *termClient) { killServer() }, "lost connection"},
		"client SIGTERM": {func(c *termClient) { c.cmd.Process.Signal(syscall.SIGTERM) }, "lost connection"},
		"client SIGHUP":  {func(c *termClient) { c.cmd.Process.Signal(syscall.SIGHUP) }, "lost connection"},
		"client SIGINT":  {func(c *termClient) { c.cmd.Process.Signal(syscall.SIGINT) }, "lost connection"},
	} {
		t.Run(name, func(t *testing.T) {
			e.t = t
			// Start from nothing, so the session under test is always 0.
			e.killAll()
			e.mustRun("ps")
			c := e.attachTerm(24, 80)
			c.expect("dsh$ ")
			if c.cooked() {
				t.Fatal("terminal should be raw while attached")
			}
			s.end(c)
			c.expect(s.expect)
			c.waitExit()
			if !c.cooked() {
				t.Fatal("terminal left in raw mode")
			}
			if !c.contains("\x1b[?25h") {
				t.Fatal("cursor not re-shown")
			}
		})
	}
}

// A termination signal must still restore the terminal while the client is
// stuck writing a large replay to a terminal that is not reading.
func TestSignalWhileTerminalIsStalled(t *testing.T) {
	e := newEnv(t)
	a := e.attachTerm(24, 80, "-n", "big")
	a.expect("dsh$ ")
	a.send("head -c 12000000 /dev/zero | tr '\\0' x; echo flooded-$((1+1))\r")
	a.expect("flooded-2")
	a.detach()

	c := e.startTerm(false, 24, 80, "attach", "big")
	for i := 0; i < 100 && c.cooked(); i++ { // wait for raw mode
		time.Sleep(20 * time.Millisecond)
	}
	if c.cooked() {
		t.Fatal("client never went raw")
	}
	time.Sleep(300 * time.Millisecond) // now blocked writing the replay
	c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.exited:
	case <-time.After(6 * time.Second):
		t.Fatal("client did not exit on SIGTERM")
	}
	if !c.cooked() {
		t.Fatal("terminal left in raw mode")
	}
}

// Resizing: a reattach applies the new size at once and makes the foreground
// program notice even when the size did not change.
func TestReattachResizesAndSignalsApplication(t *testing.T) {
	e := newEnv(t)
	c := e.attachTerm(24, 80, "-n", "w")
	c.expect("dsh$ ")
	c.send(`sh -c 'trap "echo size-\$(stty size)" WINCH; echo ready; while :; do sleep 0.1; done'` + "\r")
	c.expect("ready")
	c.detach()

	c = e.attachTerm(30, 100, "attach", "w")
	c.expect("size-30 100")
	c.detach()

	// Same size again: the replay shows the earlier line once, and the
	// forced SIGWINCH adds a second.
	c = e.attachTerm(30, 100, "attach", "w")
	deadline := time.Now().Add(5 * time.Second)
	for c.count("size-30 100") < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := c.count("size-30 100"); n != 2 {
		t.Fatalf("size-30 100 seen %d times, want 2 (replay + redraw signal)", n)
	}

	// A live resize while attached, with a pixel size.
	if err := pty.Setsize(c.ptmx, &pty.Winsize{Rows: 50, Cols: 120, X: 960, Y: 800}); err != nil {
		t.Fatal(err)
	}
	c.expect("size-50 120")
}

func TestPixelSizeReachesSession(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	e := newEnv(t)
	c := e.attachTerm(24, 80)
	c.expect("dsh$ ")
	if err := pty.Setsize(c.ptmx, &pty.Winsize{Rows: 24, Cols: 80, X: 640, Y: 480}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	c.send(`python3 -c "import fcntl,termios,struct;print('ws', struct.unpack('HHHH', fcntl.ioctl(0, termios.TIOCGWINSZ, bytes(8))))"` + "\r")
	c.expect("ws (24, 80, 640, 480)")
}

// updateEnv is an environment with a (fake) newer release 9.9.9 on offer from
// a local server, whose install script just records where it was told to
// install.
func updateEnv(t *testing.T) (e *env, marker string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/9.9.9", http.StatusFound)
	})
	e = newEnv(t)
	marker = filepath.Join(e.root, "marker")
	mux.HandleFunc("/install.sh", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "#!/bin/sh\necho \"script-ran dir=$MOOR_INSTALL_DIR\" | tee %s\n", marker)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e.setVar("XDG_CACHE_HOME", filepath.Join(e.root, "cache"))
	e.setVar("MOOR_UPDATE_URL", srv.URL+"/releases/latest")
	e.setVar("MOOR_UPDATE_SCRIPT_URL", srv.URL+"/install.sh")
	e.setVar("MOOR_NO_UPDATE_CHECK", "")
	return e, marker
}

func (e *env) cacheFile() string { return filepath.Join(e.root, "cache", "moor", "update.json") }

// A newer release is found in the background and announced after a later
// command, once, and only on a terminal.
func TestUpdateNotice(t *testing.T) {
	e, _ := updateEnv(t)

	// Not a terminal: no notice, and no check either.
	if out := e.mustRun("ps"); strings.Contains(out, "available") {
		t.Fatalf("notice without a terminal: %q", out)
	}
	if _, err := os.Stat(e.cacheFile()); err == nil {
		t.Fatal("checked for updates without a terminal")
	}

	// The first command on a terminal starts the check but knows nothing yet.
	c := e.attachTerm(24, 80, "ps")
	c.waitExit()
	if c.contains("available") {
		t.Fatal("notice before anything was fetched")
	}
	var data []byte
	for i := 0; i < 200 && !strings.Contains(string(data), "9.9.9"); i++ {
		time.Sleep(25 * time.Millisecond)
		data, _ = os.ReadFile(e.cacheFile())
	}
	if !strings.Contains(string(data), "9.9.9") {
		t.Fatalf("background check did not cache the release: %q", data)
	}

	// The next one says so, and how to update, once.
	c = e.attachTerm(24, 80, "ps")
	c.waitExit()
	if !c.contains("moor 9.9.9 is available (you have 0.1.0)") || !c.contains("moor update") {
		t.Fatalf("no notice:\n%q", c.output.String())
	}
	c = e.attachTerm(24, 80, "ps")
	c.waitExit()
	if c.contains("available") {
		t.Fatal("notice repeated within a day")
	}
}

// After a session the user is asked; yes runs the install script next to the
// running binary, no is remembered, and "moor update" needs no question.
func TestUpdateOffer(t *testing.T) {
	e, marker := updateEnv(t)
	prime := func() {
		os.MkdirAll(filepath.Dir(e.cacheFile()), 0o700)
		fresh := time.Now().Format(time.RFC3339)
		os.WriteFile(e.cacheFile(), []byte(`{"checked":"`+fresh+`","latest":"9.9.9"}`), 0o600)
	}
	e.mustRun("-n", "w", "run")
	detach := func(c *termClient) {
		c.expect("dsh$ ")
		c.send("\x1c\x1c")
		c.expect("[moor: detached")
	}

	// No: nothing runs, and it is not asked again for this release.
	prime()
	c := e.attachTerm(24, 80, "attach", "w")
	detach(c)
	c.expect("moor 9.9.9 is available (you have 0.1.0).")
	c.expect("Update now? [y/N]")
	c.send("n\r")
	c.expect("moor update")
	c.waitExit()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the install script ran after answering no")
	}
	if data, _ := os.ReadFile(e.cacheFile()); !strings.Contains(string(data), `"declined":"9.9.9"`) {
		t.Fatalf("decline not remembered: %s", data)
	}
	c = e.attachTerm(24, 80, "attach", "w")
	detach(c)
	c.waitExit()
	if c.contains("available") {
		t.Fatal("asked again about a declined release")
	}

	// An attach that is refused is not a session worth interrupting.
	prime()
	e.setVar("MOOR_SESSION", "0")
	c = e.attachTerm(24, 80, "attach", "w")
	c.expect("cannot attach session 0 from inside itself")
	c.waitExit()
	if c.contains("available") {
		t.Fatal("offered an update after a refused attach")
	}
	e.setVar("MOOR_SESSION", "")

	// Yes: the script runs, told to install where this binary lives.
	prime()
	c = e.attachTerm(24, 80, "attach", "w")
	detach(c)
	c.expect("Update now? [y/N]")
	c.send("y\r")
	c.expect("script-ran dir=" + filepath.Dir(binary))
	c.waitExit()

	// "moor update" goes straight to it.
	os.Remove(marker)
	if out := e.mustRun("update"); !strings.Contains(out, "script-ran dir=") {
		t.Fatalf("moor update output %q", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("moor update did not run the script")
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

// "--" marks where the command begins, so its own options are left alone.
func TestCommandBoundary(t *testing.T) {
	e := newEnv(t)
	if out := e.mustRun("run", "--", "sleep", "60"); !strings.Contains(out, "started session 0 (sleep)") {
		t.Fatalf("run -- output %q", out)
	}
	if out := e.mustRun("run", "-n", "build", "--", "sh", "-c", "echo it-works-$((20+22)); sleep 60"); !strings.Contains(out, "(build)") {
		t.Fatalf("run -n output %q", out)
	}
	// Options after the command's name are the command's, not moor's.
	if out := e.mustRun("run", "sleep", "-n", "60"); !strings.Contains(out, "(sleep-1)") {
		t.Fatalf("run output %q", out)
	}
	for _, bad := range [][]string{{"run", "-x", "sleep"}, {"-n", "a", "run", "-n", "b", "--", "sleep", "9"}, {"run", "-n"}} {
		if out, err := e.run(bad...); err == nil {
			t.Fatalf("moor %v succeeded: %s", bad, out)
		}
	}
	c := e.attachTerm(24, 80, "start", "-n", "shown", "--", "echo", "boundary-$((1+1))")
	c.expect("boundary-")
	c.detach()
	c = e.attachTerm(24, 80, "attach", "build")
	c.expect("it-works-42")
}

// Programs in a session must see default signal dispositions, so Ctrl-C and
// hangups work as in any terminal.
func TestCtrlCInterruptsForegroundProgram(t *testing.T) {
	e := newEnv(t)
	c := e.attachTerm(24, 80)
	c.expect("dsh$ ")
	c.send("sleep 4322\r")
	time.Sleep(300 * time.Millisecond)
	c.send("\x03")
	c.send("echo back-$((1+1))\r")
	c.expect("back-2")
}

// Ending a session must take whatever it is running with it, whether or not
// the shell passes SIGHUP on to its jobs (bash started as sh does not).
func TestKillStopsForegroundJob(t *testing.T) {
	e := newEnv(t)
	e.mustRun("run", "sleep", "4321")
	time.Sleep(500 * time.Millisecond) // typed into the shell once it is up
	running := func() bool {
		out, _ := exec.Command("ps", "-A", "-o", "args=").Output()
		return strings.Contains("\n"+string(out), "\nsleep 4321")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !running() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !running() {
		t.Fatal("the command never started")
	}
	e.mustRun("kill", "0")
	deadline = time.Now().Add(5 * time.Second)
	for running() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if running() {
		t.Fatal("sleep survived the end of its session")
	}
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
	for id, want := range map[int]string{0: "0 project ", 1: "1 — ", 2: "2 — "} {
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

func TestRenameAndPsColumns(t *testing.T) {
	e := newEnv(t)
	e.mustRun("run", "cd /usr && sleep 60")
	e.mustRun("-n", "taken", "run")
	if out := e.mustRun("rename", "0", "renamed"); !strings.Contains(out, "renamed session 0 to renamed") {
		t.Fatalf("rename output %q", out)
	}
	for _, bad := range [][]string{{"rename", "0", "taken"}, {"rename", "0", "7"}, {"rename", "0", "has space"}, {"rename", "nope", "x"}} {
		if out, err := e.run(bad...); err == nil {
			t.Fatalf("moor %v succeeded: %s", bad, out)
		}
	}
	// The command is typed into the shell once it is up; wait for its cd.
	var ps string
	for i := 0; i < 100; i++ {
		ps = e.ps()
		if strings.HasSuffix(psLine(ps, 0), " /usr") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h := strings.Join(strings.Fields(strings.SplitN(ps, "\n", 2)[0]), " "); h != "ID NAME STATE PID AGE CWD" {
		t.Fatalf("unexpected ps header %q", h)
	}
	f := strings.Fields(psLine(ps, 0))
	if len(f) != 6 || f[1] != "renamed" || f[2] != "detached" || !strings.HasSuffix(f[4], "s") || f[5] != "/usr" {
		t.Fatalf("ps line %q", psLine(ps, 0))
	}
	// The new name works everywhere the old one did.
	e.mustRun("kill", "renamed")
}

func TestAttachWithoutArgument(t *testing.T) {
	e := newEnv(t)
	if out, err := e.run("attach"); err == nil || !strings.Contains(out, "no sessions") {
		t.Fatalf("attach with no sessions: %v, %q", err, out)
	}
	attachedTo := func(args ...string) string {
		c := e.attachTerm(24, 80, args...)
		c.expect("dsh$ ")
		var attached string
		for _, l := range strings.Split(e.ps(), "\n")[1:] {
			if f := strings.Fields(l); len(f) > 2 && strings.HasPrefix(f[2], "attached") {
				attached = f[0]
			}
		}
		c.detach()
		return attached
	}

	e.mustRun("-n", "only", "run")
	for _, args := range [][]string{{"attach"}, {"a"}, {"-a"}, {"-a", "-r"}, {"-a", "-r", "only"}, {"-r", "-a", "only"}, {"-a", "only", "-r"}} {
		if got := attachedTo(args...); got != "0" {
			t.Fatalf("moor %v with one session attached to %q", args, got)
		}
	}

	// With several, session 0 is the default; an explicit target overrides it.
	e.mustRun("-n", "second", "run")
	e.mustRun("-n", "third", "run")
	for args, want := range map[string]string{"attach": "0", "-a": "0"} {
		if got := attachedTo(args); got != want {
			t.Fatalf("moor %s with three sessions attached to %q, want %s", args, got, want)
		}
	}
	if got := attachedTo("attach", "third"); got != "2" {
		t.Fatalf("moor attach third attached to %q", got)
	}
	if got := attachedTo("-a", "1"); got != "1" {
		t.Fatalf("moor -a 1 attached to %q", got)
	}

	// Inside a session, the default skips that session itself.
	e.setVar("MOOR_SESSION", "0")
	if got := attachedTo("attach"); got != "1" {
		t.Fatalf("moor attach inside session 0 attached to %q, want 1", got)
	}
	e.setVar("MOOR_SESSION", "")

	// Without session 0, the lowest remaining ID.
	e.mustRun("kill", "0")
	if got := attachedTo("attach"); got != "1" {
		t.Fatalf("moor attach without session 0 attached to %q, want 1", got)
	}
}

func TestCurrentDirectorySession(t *testing.T) {
	e := newEnv(t)
	proj := filepath.Join(e.root, "My Proj")
	other := filepath.Join(e.root, "other")
	os.Mkdir(proj, 0o755)
	os.Mkdir(other, 0o755)

	// Nothing yet: "moor ." creates a session named after the directory.
	e.cwd = proj
	c := e.attachTerm(24, 80, ".")
	c.expect("dsh$ ")
	if l := psLine(e.ps(), 0); !strings.HasPrefix(l, "0 My-Proj attached ") {
		t.Fatalf("ps line %q", l)
	}
	c.detach()

	// A second run attaches to it instead of creating another, whether
	// spelled "." or "cwd", and -r watches.
	c = e.attachTerm(24, 80, "cwd")
	c.expect("dsh$ ")
	w := e.attachTerm(24, 80, "-r", ".")
	w.expect("dsh$ ")
	if got := strings.Count(e.ps(), "\n"); got != 2 { // header + one session
		t.Fatalf("expected a single session:\n%s", e.ps())
	}
	w.detach()
	c.detach()

	// Other directories get their own session; two in one directory are
	// ambiguous.
	e.cwd = other
	e.mustRun("run")
	if l := psLine(e.ps(), 1); !strings.HasPrefix(l, "1 — ") { // unnamed: not the "My-Proj" one
		t.Fatalf("ps line %q", l)
	}
	e.mustRun("run")
	out, err := e.run(".")
	if err == nil || !strings.Contains(out, "several sessions started in") {
		t.Fatalf("moor . with two sessions: %v, %q", err, out)
	}
	if out, err := e.run("-n", "x", "."); err == nil {
		t.Fatalf("-n with an existing session accepted: %q", out)
	}
}

func TestPsWithUnreadableMetadata(t *testing.T) {
	e := newEnv(t)
	e.mustRun("run")
	if err := os.WriteFile(filepath.Join(e.root, "moor", "0", "session.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l := psLine(e.ps(), 0); l != "0 ? detached - - -" {
		t.Fatalf("ps line %q", l)
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

func (e *env) serverPID(id int) int {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.root, "moor", fmt.Sprint(id), "session.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	var m struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(data, &m); err != nil || m.PID <= 0 {
		e.t.Fatalf("no server pid in %s", data)
	}
	return m.PID
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

// A session started from an activated virtualenv gets the same environment,
// even when the user's startup files put other directories ahead of it.
func TestSessionInheritsActivatedVirtualenv(t *testing.T) {
	for _, tc := range []struct{ shell, prompt string }{
		{"/bin/bash", "1"}, {"/bin/zsh", "1"}, {"/bin/bash", "0"}, {"/bin/zsh", "0"}, // MOOR_PROMPT
	} {
		shell := tc.shell
		t.Run(filepath.Base(shell)+"-prompt"+tc.prompt, func(t *testing.T) {
			if _, err := os.Stat(shell); err != nil {
				t.Skip(shell + " not installed")
			}
			e := newEnv(t)
			e.setVar("SHELL", shell)
			e.setVar("MOOR_PROMPT", tc.prompt)
			venv := filepath.Join(e.root, "venv")
			shadow := filepath.Join(e.root, "shadow")
			for dir, out := range map[string]string{filepath.Join(venv, "bin"): "from-venv", shadow: "from-shadow"} {
				os.MkdirAll(dir, 0o755)
				os.WriteFile(filepath.Join(dir, "mytool"), []byte("#!/bin/sh\necho "+out+"\n"), 0o755)
			}
			os.WriteFile(filepath.Join(venv, "bin", "activate"), []byte(
				"deactivate() { unset VIRTUAL_ENV; }\nVIRTUAL_ENV="+venv+"\nexport VIRTUAL_ENV\nPATH=\"$VIRTUAL_ENV/bin:$PATH\"\nexport PATH\n"), 0o644)
			// moor is started from a shell where the venv is active.
			e.setVar("VIRTUAL_ENV", venv)
			e.setVar("PATH", filepath.Join(venv, "bin")+":"+os.Getenv("PATH"))
			rc := "export PATH=\"" + shadow + ":$PATH\"\nPS1='dsh$ '\nPROMPT='dsh$ '\n"
			for _, f := range []string{".bashrc", ".zshrc"} {
				os.WriteFile(filepath.Join(e.root, f), []byte(rc), 0o600)
			}

			c := e.attachTerm(24, 80, "-n", "py")
			c.expect("dsh$ ")
			c.send("echo \"[$(mytool)] [$(command -v deactivate)] [$VIRTUAL_ENV]\"\r")
			c.expect("[from-venv] [deactivate] [" + venv + "]")
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
