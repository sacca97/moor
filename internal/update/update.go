// Package update tells the user when a newer moor release exists.
//
// It never slows a command down: at most once a day a detached background
// process asks GitHub for the latest release tag (one HEAD request, following
// no redirect, nothing identifying sent), and the answer is cached; a later
// command announces it from the cache. After an interactive session the user
// is asked whether to update, and only a "yes" runs the release's install
// script, the same one as "curl ... | sh". MOOR_NO_UPDATE_CHECK=1 turns the
// check off; "moor update" installs on request.
package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	defaultURL    = "https://github.com/sacca97/moor/releases/latest"
	defaultScript = "https://github.com/sacca97/moor/releases/latest/download/install.sh"
	installCmd    = "curl -fsSL " + defaultScript + " | sh"
	interval      = 24 * time.Hour
)

// RefreshCommand is the hidden command that runs Refresh in the background.
const RefreshCommand = "__update-check"

type state struct {
	Checked  time.Time `json:"checked"`  // when a refresh was last started
	Latest   string    `json:"latest"`   // newest release tag seen
	Notified time.Time `json:"notified"` // when the notice was last shown
	Declined string    `json:"declined"` // release the user said no to updating to
}

var releaseVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func statePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "moor", "update.json"), nil
}

func load(path string) (s state) {
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &s)
	}
	return s
}

// save writes the state through a uniquely named temporary file, so
// concurrent writers never truncate each other's half-written data.
func save(path string, s state) {
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	data, _ := json.Marshal(s)
	f, err := os.CreateTemp(filepath.Dir(path), "update-*.tmp")
	if err != nil {
		return
	}
	_, werr := f.Write(data) // CreateTemp makes the file 0600
	if f.Close() != nil || werr != nil || os.Rename(f.Name(), path) != nil {
		os.Remove(f.Name())
	}
}

// modify runs a read-modify-write of the state under a file lock, so that
// foreground commands and the background refresh never lose each other's
// updates. fn returns whether the state changed and must be saved. If the
// lock cannot be taken, fn still runs, unlocked.
func modify(path string, fn func(s *state) bool) {
	if os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		if f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600); err == nil {
			defer f.Close()
			for {
				err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
				if err != unix.EINTR {
					break
				}
			}
			if err == nil {
				defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
			}
		}
	}
	s := load(path)
	if fn(&s) {
		save(path, s)
	}
}

// Check returns the release to tell the user about, if a newer one than
// current is known and they have not been told in the last day or declined
// it. It also starts a background refresh when the cached answer is a day
// old. It is cheap: one small file read, and it does nothing at all for
// development builds, when disabled, or when stderr is not a terminal.
func Check(current string) string {
	if os.Getenv("MOOR_NO_UPDATE_CHECK") != "" || !releaseVersion.MatchString(current) ||
		!term.IsTerminal(int(os.Stderr.Fd())) {
		return ""
	}
	path, err := statePath()
	if err != nil {
		return ""
	}
	return check(current, path, time.Now(), spawnRefresh)
}

func check(current, path string, now time.Time, spawn func()) string {
	var latest string
	var refresh bool
	modify(path, func(s *state) bool {
		if now.Sub(s.Checked) >= interval {
			// Record the attempt first, so concurrent commands do not all
			// spawn one, and a failing network is retried only once a day.
			s.Checked = now
			refresh = true
		}
		if Newer(s.Latest, current) && s.Latest != s.Declined && now.Sub(s.Notified) >= interval {
			s.Notified = now
			latest = s.Latest
		}
		return refresh || latest != ""
	})
	if refresh {
		spawn()
	}
	return latest
}

// Offer tells the user that release latest is available. When both stdin and
// stderr are terminals it asks whether to update now and, on a "yes", runs
// the install script; a "no" is remembered until a newer release appears.
func Offer(latest, current string) {
	path, _ := statePath()
	offer(latest, current, path, os.Stdin, term.IsTerminal(int(os.Stdin.Fd())), os.Stderr, Install)
}

func offer(latest, current, path string, in io.Reader, interactive bool, out io.Writer, install func() error) {
	fmt.Fprintf(out, "moor %s is available (you have %s).\n", latest, current)
	if !interactive {
		fmt.Fprintln(out, "Update with: moor update")
		return
	}
	fmt.Fprint(out, "Update now? [y/N] ")
	line, err := readLine(in)
	if err != nil && strings.TrimSpace(line) == "" {
		// End of input (Ctrl-D, closed terminal) is not an answer.
		fmt.Fprintln(out)
		return
	}
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		if path != "" {
			modify(path, func(s *state) bool {
				s.Declined = latest
				return true
			})
		}
		fmt.Fprintln(out, "Not now. 'moor update' installs it whenever you like.")
		return
	}
	if err := install(); err != nil {
		fmt.Fprintf(out, "moor: update failed: %v\nTo update by hand:\n  %s\n", err, installCmd)
	}
}

// readLine reads up to the first newline, one byte at a time so that nothing
// the user typed ahead is consumed beyond it.
func readLine(r io.Reader) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < 1024 {
		if _, err := io.ReadFull(r, b); err != nil {
			return string(line), err
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	return string(line), nil
}

// maxScript bounds the install script we are willing to download.
const maxScript = 1 << 20

// Install downloads the latest release's install script and runs it, putting
// the new binary next to the one that is running. The script has the version
// and the checksums of its archives built in and refuses anything that does
// not match.
func Install() error {
	url := os.Getenv("MOOR_UPDATE_SCRIPT_URL") // for tests and mirrors
	if url == "" {
		url = defaultScript
	}
	return install(url, os.Stdin, os.Stdout, os.Stderr)
}

func install(url string, stdin io.Reader, stdout, stderr io.Writer) error {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading the install script: %s", resp.Status)
	}
	script, err := io.ReadAll(io.LimitReader(resp.Body, maxScript+1))
	if err != nil {
		return err
	}
	// An empty script would "succeed" without installing anything, and a
	// cut-off one is not the script we meant to run.
	switch {
	case len(script) == 0:
		return fmt.Errorf("the install script from %s is empty", url)
	case len(script) > maxScript:
		return fmt.Errorf("the install script from %s is too large", url)
	}
	f, err := os.CreateTemp("", "moor-install-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(script); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	cmd := exec.Command("sh", f.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = os.Environ()
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			cmd.Env = append(cmd.Env, "MOOR_INSTALL_DIR="+filepath.Dir(exe))
		}
	}
	return cmd.Run()
}

func spawnRefresh() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer devnull.Close()
	cmd := exec.Command(exe, RefreshCommand)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

// Refresh asks GitHub for the latest release tag and caches it. It is the
// body of the background process.
func Refresh() error {
	url := os.Getenv("MOOR_UPDATE_URL") // for tests and mirrors
	if url == "" {
		url = defaultURL
	}
	path, err := statePath()
	if err != nil {
		return err
	}
	return refresh(url, path)
}

func refresh(url, path string) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		// The answer is the redirect itself: .../releases/tag/<version>.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Head(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	_, tag, ok := strings.Cut(loc, "/tag/")
	if !ok || !releaseVersion.MatchString(tag) {
		return fmt.Errorf("unexpected response from %s: %d %q", url, resp.StatusCode, loc)
	}
	modify(path, func(s *state) bool {
		// Never go backwards: a stale mirror must not hide a newer release
		// that was already seen.
		if s.Latest == "" || Newer(tag, s.Latest) {
			s.Latest = tag
			return true
		}
		return false
	})
	return nil
}

// Newer reports whether release version a is newer than b. Both must be
// X.Y.Z; anything else is never newer.
func Newer(a, b string) bool {
	if !releaseVersion.MatchString(a) || !releaseVersion.MatchString(b) {
		return false
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range pa {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return false
}
