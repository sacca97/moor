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
	"bufio"
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

func save(path string, s state) {
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	data, _ := json.Marshal(s)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, path)
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
	now := time.Now()
	s := load(path)
	if now.Sub(s.Checked) >= interval {
		// Record the attempt first, so concurrent commands do not all spawn
		// one, and a failing network is retried only once a day.
		s.Checked = now
		save(path, s)
		spawnRefresh()
	}
	if !Newer(s.Latest, current) || s.Latest == s.Declined || now.Sub(s.Notified) < interval {
		return ""
	}
	s.Notified = now
	save(path, s)
	return s.Latest
}

// Offer tells the user that release latest is available. When both stdin and
// stderr are terminals it asks whether to update now and, on a "yes", runs
// the install script; a "no" is remembered until a newer release appears.
func Offer(latest, current string) {
	fmt.Fprintf(os.Stderr, "moor %s is available (you have %s).\n", latest, current)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "Update with:\n  %s\n", installCmd)
		return
	}
	fmt.Fprint(os.Stderr, "Update now? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		if path, err := statePath(); err == nil {
			s := load(path)
			s.Declined = latest
			save(path, s)
		}
		fmt.Fprintln(os.Stderr, "Not now. 'moor update' installs it whenever you like.")
		return
	}
	if err := Install(); err != nil {
		fmt.Fprintf(os.Stderr, "moor: update failed: %v\nTo update by hand:\n  %s\n", err, installCmd)
	}
}

// Install downloads the latest release's install script and runs it, putting
// the new binary next to the one that is running. The script has the version
// and the checksums of its archives built in and refuses anything that does
// not match.
func Install() error {
	url := os.Getenv("MOOR_UPDATE_SCRIPT_URL") // for tests and mirrors
	if url == "" {
		url = defaultScript
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading the install script: %s", resp.Status)
	}
	script, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
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
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
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
	path, err := statePath()
	if err != nil {
		return err
	}
	s := load(path)
	s.Latest = tag
	save(path, s)
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
