// Package cli implements moor's command line.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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
  moor [-n NAME] start COMMAND   start a shell, run COMMAND in it, attach
  moor [-n NAME] run COMMAND     same as start, but stay detached
  moor attach ID|NAME            attach to a session (also: moor -a ID|NAME)
  moor ps                        list sessions
  moor kill ID|NAME              terminate a session

Press Ctrl-\ twice to detach. The shell keeps running.
`

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
	var nameSet, attachSet bool

	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Print(usage)
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
			return usageError("-a takes no other arguments")
		}
		return cmdAttach(attach)
	}
	if len(rest) == 0 {
		return cmdNew(name, "", true)
	}

	switch rest[0] {
	case "start", "run":
		command := joinCommand(rest[1:])
		return cmdNew(name, command, rest[0] == "start")
	case "attach", "a":
		if nameSet || len(rest) != 2 {
			return usageError("usage: moor attach ID|NAME")
		}
		return cmdAttach(rest[1])
	case "ps", "ls", "list":
		if nameSet || len(rest) != 1 {
			return usageError("usage: moor ps")
		}
		return cmdPs(os.Stdout)
	case "kill":
		if nameSet || len(rest) != 2 {
			return usageError("usage: moor kill ID|NAME")
		}
		return cmdKill(rest[1])
	case "help":
		fmt.Print(usage)
		return nil
	}
	return usageError(fmt.Sprintf("unknown command %q (to run a program, use: moor start %s)", rest[0], rest[0]))
}

func cmdNew(name, command string, attach bool) error {
	fd := int(os.Stdin.Fd())
	if attach && !term.IsTerminal(fd) {
		return errors.New("stdin is not a terminal (use 'moor run' to start a detached session)")
	}
	rows, cols := client.TermSize(fd)
	m, err := session.Create(session.CreateOptions{
		Name:    name,
		Command: command,
		Rows:    rows,
		Cols:    cols,
	})
	if err != nil {
		return err
	}
	if !attach {
		fmt.Printf("started %s\n", sessionLabel(m.ID, m.Name))
		return nil
	}
	return attachTo(m.ID, m.Name)
}

func cmdAttach(target string) error {
	s, err := session.Resolve(target)
	if err != nil {
		return err
	}
	if s.Attached {
		return fmt.Errorf("session %d is already attached", s.ID)
	}
	return attachTo(s.ID, s.Name)
}

func attachTo(id int, name string) error {
	if os.Getenv("MOOR_SESSION") == strconv.Itoa(id) {
		return fmt.Errorf("cannot attach session %d from inside itself", id)
	}
	o, err := client.Attach(session.SocketPath(id))
	if errors.Is(err, client.ErrBusy) {
		return fmt.Errorf("session %d is already attached", id)
	}
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
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tPID")
	for _, s := range sessions {
		status := "detached"
		if s.Attached {
			status = "attached"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\n", s.ID, s.Name, status, s.PID)
	}
	return tw.Flush()
}

func cmdKill(target string) error {
	s, err := session.Resolve(target)
	if err != nil {
		return err
	}
	if s.PID <= 0 {
		return fmt.Errorf("session %d has no known server pid", s.ID)
	}
	if err := syscall.Kill(s.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("killing session %d: %w", s.ID, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(session.Dir(s.ID)); os.IsNotExist(err) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("killed %s\n", sessionLabel(s.ID, s.Name))
	return nil
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
