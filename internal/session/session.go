// Package session manages the runtime directory that describes moor
// sessions: metadata, sockets, ID allocation, naming and liveness.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Meta is the informational content of session.json. It is never trusted on
// its own to decide whether a session exists; see Probe.
type Meta struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	PID       int       `json:"pid"`
	ShellPID  int       `json:"shell_pid"`
	CreatedAt time.Time `json:"created_at"`
	CWD       string    `json:"cwd"`
	Command   string    `json:"command,omitempty"`
	Attached  bool      `json:"attached"`
}

const (
	metaFile   = "session.json"
	socketFile = "socket"
)

// Dir returns the runtime directory of session id.
func Dir(id int) string {
	return filepath.Join(Root(), strconv.Itoa(id))
}

// SocketPath returns the Unix socket path of session id.
func SocketPath(id int) string {
	return filepath.Join(Dir(id), socketFile)
}

// ReadMeta reads session.json from a session directory.
func ReadMeta(dir string) (Meta, error) {
	var m Meta
	data, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

// WriteMeta atomically replaces session.json in a session directory.
func WriteMeta(dir string, m Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".session-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, metaFile))
}
