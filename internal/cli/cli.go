// Package cli implements moor's command line.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/sacca/moor/internal/client"
	"github.com/sacca/moor/internal/server"
	"github.com/sacca/moor/internal/session"
)

const usage = `moor - moor a shell so it stays alive, then attach and detach at will

Usage:
  moor [-n NAME]                 start a shell and attach to it
  moor [-n NAME] start [--] COMMAND [ARGS...]
                                 start a shell, run COMMAND in it, attach
  moor [-n NAME] run [--] COMMAND [ARGS...]
                                 same as start, but stay detached
  moor [-r] [-n NAME] . | cwd    attach to the session started in this directory,
                                 or create one named after it
  moor attach [-r] [ID|NAME]     attach to a session (also: moor a, moor -a ID|NAME);
                                 with no argument, attach to the only session
  moor ps                        list sessions
  moor rename ID|NAME NEWNAME    rename a session
  moor kill ID|NAME              terminate a session
  moor --version                 print the version

Press Ctrl-\ twice, or Ctrl-b d, to detach. The shell keeps running.

A session can have several terminals attached. The newest one controls it; the
others are read-only until it detaches. With -r, a terminal only watches.
`

// Version is the moor version, set by the main package.
var Version = "dev"

type usageError string

func (e usageError) Error() string { return string(e) }

// Main runs moor with the given arguments (without the program name) and
// returns the exit code.
func Main(args []string) int {
	if len(args) > 0 && args[0] == "__serve" {
		return serve(args[1:])
	}
	err := run(args)
	if err == nil {
		return 0
	}
	fmt.Fprintf(os.Stderr, "moor: %v\n", err)
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprint(os.Stderr, "\n"+usage)
		return 2
	}
	return 1
}

func run(args []string) error {
	var name, attach string
	var nameSet, attachSet, readOnly bool

	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Print(usage)
			return nil
		case a == "--version":
			fmt.Println("moor", Version)
			return nil
		case a == "-n" || a == "--name":
			if i+1 >= len(args) {
				return usageError(a + " requires a name")
			}
			i++
			name, nameSet = args[i], true
		case strings.HasPrefix(a, "--name="):
			name, nameSet = strings.TrimPrefix(a, "--name="), true
		case a == "-a" || a == "--attach":
			if i+1 >= len(args) {
				return usageError(a + " requires a session ID or name")
			}
			i++
			attach, attachSet = args[i], true
		case strings.HasPrefix(a, "--attach="):
			attach, attachSet = strings.TrimPrefix(a, "--attach="), true
		case a == "-r" || a == "--read-only":
			readOnly = true
		case a == "--":
			i++
			goto parsed
		case strings.HasPrefix(a, "-") && a != "-":
			return usageError("unknown option " + a)
		default:
			goto parsed
		}
	}
parsed:
	rest := args[i:]

	if nameSet {
		if err := session.ValidateName(name); err != nil {
			return err
		}
	}
	if attachSet {
		if nameSet || len(rest) > 0 {
			return usageError("-a takes no other arguments except -r")
		}
		return cmdAttach(attach, readOnly)
	}
	if readOnly && (len(rest) == 0 || (rest[0] != "attach" && rest[0] != "a" && rest[0] != "." && rest[0] != "cwd")) {
		return usageError("-r only applies to attach")
	}
	if len(rest) == 0 {
		return cmdNew(name, "", true)
	}

	switch rest[0] {
	case "start", "run":
		startName, cmdArgs, err := parseStartArgs(rest[1:])
		if err != nil {
			return err
		}
		if startName != "" {
			if nameSet {
				return usageError("the session name was given twice")
			}
			if err := session.ValidateName(startName); err != nil {
				return err
			}
			name = startName
		}
		return cmdNew(name, joinCommand(cmdArgs), rest[0] == "start")
	case ".", "cwd":
		if len(rest) != 1 {
			return usageError("usage: moor [-r] [-n NAME] .")
		}
		return cmdHere(name, nameSet, readOnly)
	case "attach", "a":
		targets := rest[1:]
		if len(targets) > 0 && (targets[0] == "-r" || targets[0] == "--read-only") {
			readOnly, targets = true, targets[1:]
		}
		if nameSet || len(targets) > 1 {
			return usageError("usage: moor attach [-r] [ID|NAME]")
		}
		if len(targets) == 0 {
			return cmdAttachOnly(readOnly)
		}
		return cmdAttach(targets[0], readOnly)
	case "ps", "ls", "list":
		if nameSet || len(rest) != 1 {
			return usageError("usage: moor ps")
		}
		return cmdPs(os.Stdout)
	case "rename":
		if nameSet || len(rest) != 3 {
			return usageError("usage: moor rename ID|NAME NEWNAME")
		}
		return cmdRename(rest[1], rest[2])
	case "kill":
		if nameSet || len(rest) != 2 {
			return usageError("usage: moor kill ID|NAME")
		}
		return cmdKill(rest[1])
	case "version":
		fmt.Println("moor", Version)
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	}
	return usageError(fmt.Sprintf("unknown command %q (to run a program, use: moor start %s)", rest[0], rest[0]))
}

func cmdNew(name, command string, attach bool) error {
	return newSession(session.CreateOptions{Name: name, Command: command}, attach)
}

// cmdHere attaches to the session started in the current directory. Several
// are listed for the user to choose from; none means a new one is created,
// named after the directory.
func cmdHere(name string, nameSet, readOnly bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	sessions, err := session.List()
	if err != nil {
		return err
	}
	var here []session.Meta
	for _, s := range sessions {
		if sameDir(s.CWD, cwd) {
			here = append(here, s)
		}
	}
	switch len(here) {
	case 0:
		if readOnly {
			return fmt.Errorf("no session started in %s to watch", cwd)
		}
		return newSession(session.CreateOptions{Name: name, DefaultName: session.DirName(cwd)}, true)
	case 1:
		if nameSet {
			return fmt.Errorf("a session already exists for %s (%s); -n only applies when creating one", cwd, sessionLabel(here[0].ID, here[0].Name))
		}
		return attachTo(here[0].ID, here[0].Name, readOnly)
	}
	printSessions(os.Stderr, here)
	return fmt.Errorf("several sessions started in %s; say which one: moor attach ID|NAME", cwd)
}

// sameDir reports whether two paths name the same directory, looking through
// symlinks where it can.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func newSession(opts session.CreateOptions, attach bool) error {
	fd := int(os.Stdin.Fd())
	if attach && !term.IsTerminal(fd) {
		return errors.New("stdin is not a terminal (use 'moor run' to start a detached session)")
	}
	size := client.TermSize(fd)
	opts.Rows, opts.Cols = size.Rows, size.Cols
	m, err := session.Create(opts)
	if err != nil {
		return err
	}
	if !attach {
		fmt.Printf("started %s\n", sessionLabel(m.ID, m.Name))
		return nil
	}
	return attachTo(m.ID, m.Name, false)
}

func cmdAttach(target string, readOnly bool) error {
	s, err := session.Resolve(target)
	if err != nil {
		return err
	}
	return attachTo(s.ID, s.Name, readOnly)
}

// cmdAttachOnly attaches to the only session, or explains why it cannot.
func cmdAttachOnly(readOnly bool) error {
	sessions, err := session.List()
	if err != nil {
		return err
	}
	switch len(sessions) {
	case 0:
		return errors.New("no sessions to attach to (start one with 'moor')")
	case 1:
		return attachTo(sessions[0].ID, sessions[0].Name, readOnly)
	}
	printSessions(os.Stderr, sessions)
	return errors.New("several sessions; say which one: moor attach ID|NAME")
}

func cmdRename(target, newName string) error {
	s, err := session.Resolve(target)
	if err != nil {
		return err
	}
	if err := session.Rename(s.ID, newName); err != nil {
		return err
	}
	fmt.Printf("renamed session %d to %s\n", s.ID, newName)
	return nil
}

func attachTo(id int, name string, readOnly bool) error {
	if os.Getenv("MOOR_SESSION") == strconv.Itoa(id) {
		return fmt.Errorf("cannot attach session %d from inside itself", id)
	}
	o, err := client.Attach(session.SocketPath(id), readOnly)
	if err != nil {
		return err
	}
	label := sessionLabel(id, name)
	switch o.Result {
	case client.Detached:
		fmt.Fprintf(os.Stderr, "[moor: detached from %s]\n", label)
	case client.Exited:
		fmt.Fprintf(os.Stderr, "[moor: %s exited]\n", label)
	case client.Lost:
		fmt.Fprintf(os.Stderr, "[moor: lost connection to %s]\n", label)
	}
	return nil
}

// sessionLabel names a session in messages: "session 1 (work)", or just
// "session 0" when its name is the uninformative default.
func sessionLabel(id int, name string) string {
	if session.IsDefaultName(name) {
		return fmt.Sprintf("session %d", id)
	}
	return fmt.Sprintf("session %d (%s)", id, name)
}

func cmdPs(w io.Writer) error {
	sessions, err := session.List()
	if err != nil {
		return err
	}
	printSessions(w, sessions)
	return nil
}

func printSessions(w io.Writer, sessions []session.Meta) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tPID\tAGE\tCWD")
	for _, s := range sessions {
		name := s.Name
		if session.IsDefaultName(name) {
			name = "—"
		}
		state := "detached"
		switch {
		case s.Clients == 1:
			state = "attached"
		case s.Clients > 1:
			state = fmt.Sprintf("attached (%d)", s.Clients)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\n", s.ID, name, state, s.PID, age(time.Since(s.CreatedAt)), shellCwd(s))
	}
	tw.Flush()
}

// age formats a duration with one unit: 45s, 34m, 2h, 3d.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// shellCwd is the shell's current directory where the OS lets us read it
// (Linux), else the directory the session was started in, with $HOME as ~.
func shellCwd(s session.Meta) string {
	dir := s.CWD
	if runtime.GOOS == "linux" && s.ShellPID > 0 {
		if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", s.ShellPID)); err == nil {
			dir = cwd
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "/" {
		if dir == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(dir, home+"/"); ok {
			return "~/" + rest
		}
	}
	return dir
}

func cmdKill(target string) error {
	s, err := session.Resolve(target)
	if err != nil {
		return err
	}
	if err := session.Kill(s.ID); err != nil {
		return fmt.Errorf("killing session %d: %w", s.ID, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(session.Dir(s.ID)); os.IsNotExist(err) {
			fmt.Printf("killed %s\n", sessionLabel(s.ID, s.Name))
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s is still running 5s after being told to stop", sessionLabel(s.ID, s.Name))
}

func serve(args []string) int {
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "moor: __serve is internal")
		return 2
	}
	id, err1 := strconv.Atoi(args[0])
	rows, err2 := strconv.ParseUint(args[1], 10, 16)
	cols, err3 := strconv.ParseUint(args[2], 10, 16)
	if err1 != nil || err2 != nil || err3 != nil {
		fmt.Fprintln(os.Stderr, "moor: bad __serve arguments")
		return 2
	}
	return server.Serve(id, uint16(rows), uint16(cols))
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// parseStartArgs splits what follows "start" or "run" into options and the
// command. Options (-n NAME) may come first; the command begins at "--" or at
// the first word that is not an option, and everything after it, dashes
// included, belongs to the command.
func parseStartArgs(args []string) (name string, command []string, err error) {
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return name, args[i+1:], nil
		case a == "-n" || a == "--name":
			if i+1 >= len(args) {
				return "", nil, usageError(a + " requires a name")
			}
			i++
			name = args[i]
		case strings.HasPrefix(a, "--name="):
			name = strings.TrimPrefix(a, "--name=")
		case strings.HasPrefix(a, "-") && a != "-":
			return "", nil, usageError(fmt.Sprintf("unknown option %s (put -- before a command that starts with a dash)", a))
		default:
			return name, args[i:], nil
		}
	}
	return name, nil, nil
}

// joinCommand turns the arguments after start/run into a shell command line.
// A single argument is taken as shell code verbatim; multiple arguments are
// quoted so they reach the program unchanged.
func joinCommand(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		if safeWord.MatchString(a) {
			quoted[i] = a
		} else {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}
