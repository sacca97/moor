package update

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.0", true}, {"0.1.1", "0.1.0", true}, {"1.0.0", "0.9.9", true},
		{"0.10.0", "0.9.0", true}, // numeric, not lexical
		{"0.1.0", "0.1.0", false}, {"0.1.0", "0.2.0", false},
		{"dev", "0.1.0", false}, {"0.2.0", "dev", false}, {"0.2.0", "0.1.0-dirty", false}, {"", "0.1.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCheckStateMachine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	spawns := 0
	spawn := func() { spawns++ }

	// First ever run: starts a check, knows nothing to announce.
	if got := check("0.1.0", path, t0, spawn); got != "" || spawns != 1 {
		t.Fatalf("first run: %q, %d spawns", got, spawns)
	}
	// Within the day: no second check, still nothing.
	if got := check("0.1.0", path, t0.Add(time.Hour), spawn); got != "" || spawns != 1 {
		t.Fatalf("second run: %q, %d spawns", got, spawns)
	}

	// The check found 0.2.0: announced once, then not again that day.
	save(path, state{Checked: t0, Latest: "0.2.0"})
	if got := check("0.1.0", path, t0.Add(2*time.Hour), spawn); got != "0.2.0" {
		t.Fatalf("announcement: %q", got)
	}
	if got := check("0.1.0", path, t0.Add(3*time.Hour), spawn); got != "" {
		t.Fatalf("announced twice in a day: %q", got)
	}
	// A day later it asks GitHub again and announces again.
	if got := check("0.1.0", path, t0.Add(26*time.Hour), spawn); got != "0.2.0" || spawns != 2 {
		t.Fatalf("next day: %q, %d spawns", got, spawns)
	}

	// Nothing to announce for the same or an older release, or one declined.
	for name, s := range map[string]state{
		"same":     {Checked: t0, Latest: "0.1.0"},
		"older":    {Checked: t0, Latest: "0.0.9"},
		"declined": {Checked: t0, Latest: "0.2.0", Declined: "0.2.0"},
	} {
		save(path, s)
		if got := check("0.1.0", path, t0.Add(time.Hour), spawn); got != "" {
			t.Errorf("%s: announced %q", name, got)
		}
	}
	// A declined release does not hide a newer one.
	save(path, state{Checked: t0, Latest: "0.3.0", Declined: "0.2.0"})
	if got := check("0.1.0", path, t0.Add(time.Hour), spawn); got != "0.3.0" {
		t.Errorf("newer than the declined one: %q", got)
	}
}

func TestOffer(t *testing.T) {
	run := func(input string, interactive bool) (out string, installed bool, s state) {
		path := filepath.Join(t.TempDir(), "update.json")
		var buf strings.Builder
		offer("0.2.0", "0.1.0", path, strings.NewReader(input), interactive, &buf, func() error { installed = true; return nil })
		return buf.String(), installed, load(path)
	}

	if out, installed, _ := run("", false); installed || !strings.Contains(out, "Update with: moor update") {
		t.Errorf("non-interactive: installed=%v %q", installed, out)
	}
	for _, yes := range []string{"y\n", "Y\n", "yes\n", "yes"} {
		if _, installed, s := run(yes, true); !installed || s.Declined != "" {
			t.Errorf("answer %q: installed=%v declined=%q", yes, installed, s.Declined)
		}
	}
	for _, no := range []string{"\n", "n\n", "nope\n", "maybe\n"} {
		if out, installed, s := run(no, true); installed || s.Declined != "0.2.0" || !strings.Contains(out, "moor update") {
			t.Errorf("answer %q: installed=%v declined=%q out=%q", no, installed, s.Declined, out)
		}
	}
	// End of input (Ctrl-D) is not a refusal and is not remembered.
	if _, installed, s := run("", true); installed || s.Declined != "" {
		t.Errorf("EOF: installed=%v declined=%q", installed, s.Declined)
	}

	// A failed install points at the manual command.
	var buf strings.Builder
	offer("0.2.0", "0.1.0", "", strings.NewReader("y\n"), true, &buf, func() error { return errors.New("boom") })
	if !strings.Contains(buf.String(), "update failed: boom") || !strings.Contains(buf.String(), "install.sh | sh") {
		t.Errorf("failure output %q", buf.String())
	}
}

func redirectServer(t *testing.T, location string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if location == "" {
			w.WriteHeader(http.StatusOK) // not a redirect: GitHub changed, or a proxy answered
			return
		}
		http.Redirect(w, r, location, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRefreshKeepsTheNewestRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")
	if err := refresh(redirectServer(t, "/releases/tag/0.3.0"), path); err != nil || load(path).Latest != "0.3.0" {
		t.Fatalf("first refresh: %v, %+v", err, load(path))
	}
	// A stale mirror must not take it back.
	if err := refresh(redirectServer(t, "/releases/tag/0.1.0"), path); err != nil || load(path).Latest != "0.3.0" {
		t.Fatalf("stale answer: %v, %+v", err, load(path))
	}
	if err := refresh(redirectServer(t, "/releases/tag/0.4.0"), path); err != nil || load(path).Latest != "0.4.0" {
		t.Fatalf("newer answer: %v, %+v", err, load(path))
	}
	// Anything that is not a release redirect is an error and changes nothing.
	for _, loc := range []string{"", "/releases/tag/v1", "/somewhere/else"} {
		if err := refresh(redirectServer(t, loc), path); err == nil || load(path).Latest != "0.4.0" {
			t.Errorf("location %q: %v, %+v", loc, err, load(path))
		}
	}
}

func TestInstall(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	serve := func(status int, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	var out strings.Builder
	if err := install(serve(200, "echo ran > "+marker+"; echo dir=$MOOR_INSTALL_DIR"), nil, &out, &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(marker); err != nil || !strings.Contains(out.String(), "dir=/") {
		t.Fatalf("script did not run as expected: %v %q", err, out.String())
	}
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"empty":     {200, "", "empty"},
		"not found": {404, "nope", "404"},
		"too large": {200, strings.Repeat("#", maxScript+1), "too large"},
		"failing":   {200, "exit 3", "exit status 3"},
	} {
		if err := install(serve(c.status, c.body), nil, &out, &out); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, c.want)
		}
	}
}

// Concurrent read-modify-write cycles must not lose each other's updates.
func TestModifyIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moor", "update.json")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			modify(path, func(s *state) bool {
				s.Latest += "x"
				return true
			})
		}()
	}
	wg.Wait()
	if got := len(load(path).Latest); got != 20 {
		t.Fatalf("%d of 20 updates survived", got)
	}
}
