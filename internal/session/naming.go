package session

import (
	"errors"
	"path"
	"strconv"
	"strings"
)

const (
	defaultName   = "shell"
	maxNameLength = 32
)

// builtins are shell built-ins that only change state, and wrappers are
// commands that run another command; neither names the program being run.
var (
	builtins = map[string]bool{
		"cd": true, "pushd": true, "popd": true, "export": true, "unset": true,
		"set": true, "source": true, ".": true, "alias": true, "umask": true,
		"ulimit": true, "true": true, ":": true, "local": true,
		"declare": true, "typeset": true, "setopt": true, "shopt": true,
	}
	wrappers = map[string]bool{
		"env": true, "exec": true, "command": true, "builtin": true,
		"nohup": true, "time": true, "sudo": true, "doas": true, "nice": true,
		"ionice": true, "stdbuf": true, "caffeinate": true,
	}
)

// ValidateName checks an explicit session name.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("session name must not be empty")
	}
	if len(name) > maxNameLength {
		return errors.New("session name is too long")
	}
	if Sanitize(name) != name {
		return errors.New("session name may only contain letters, digits, '.', '_' and '-'")
	}
	if _, err := strconv.Atoi(name); err == nil {
		return errors.New("session name must not be a number")
	}
	return nil
}

// Sanitize maps s onto [A-Za-z0-9._-]: other runes become '-', runs of '-'
// collapse, and leading/trailing '-' or '.' are trimmed.
func Sanitize(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-'
		if !ok || r == '-' {
			if !lastDash {
				b.WriteByte('-')
			}
			lastDash = true
			continue
		}
		b.WriteRune(r)
		lastDash = false
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > maxNameLength {
		out = strings.TrimRight(out[:maxNameLength], "-.")
	}
	return out
}

// AutoName derives a session name from a shell command line: the basename of
// the last real program in a list of commands, skipping built-ins such as cd
// and wrappers such as env. For a pipeline the first stage is used. The result
// is always a valid name; "shell" is used when nothing better is found.
func AutoName(command string) string {
	var best string
	for _, list := range splitList(command) {
		stage := list
		if i := strings.IndexByte(stage, '|'); i >= 0 {
			stage = stage[:i]
		}
		if prog := programOf(shellFields(stage)); prog != "" {
			best = prog
		}
	}
	name := Sanitize(path.Base(best))
	if best == "" || name == "" {
		name = defaultName
	}
	if _, err := strconv.Atoi(name); err == nil {
		name = "cmd-" + name
	}
	return name
}

// DirName derives a session name from a directory: its base name, cleaned up.
func DirName(dir string) string {
	name := Sanitize(path.Base(dir))
	if name == "" {
		return defaultName
	}
	if _, err := strconv.Atoi(name); err == nil {
		name = "dir-" + name
	}
	return name
}

// IsDefaultName reports whether name is the placeholder given to sessions
// with no explicit name and no command ("shell", "shell-1", ...). Such names
// carry no information, so they are left out of the prompt marker and of
// status messages.
func IsDefaultName(name string) bool {
	rest, ok := strings.CutPrefix(name, defaultName)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	n, err := strconv.Atoi(strings.TrimPrefix(rest, "-"))
	return strings.HasPrefix(rest, "-") && err == nil && n > 0
}

// UniqueName returns base, or base-1, base-2, ... whichever is not in taken.
func UniqueName(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for i := 1; ; i++ {
		n := base + "-" + strconv.Itoa(i)
		if !taken[n] {
			return n
		}
	}
}

// splitList splits a command line on ;, &&, ||, &, newlines and parentheses
// outside of quotes. "||" and "|" are distinguished so pipelines stay intact.
func splitList(s string) []string {
	var parts []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			parts = append(parts, t)
		}
		cur.Reset()
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if quote != 0 {
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' && i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			cur.WriteRune(r)
		case '\\':
			cur.WriteRune(r)
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			}
		case '&':
			// "2>&1", "<&3" and "&>file" are redirections, not separators.
			if i > 0 && (rs[i-1] == '>' || rs[i-1] == '<') || i+1 < len(rs) && rs[i+1] == '>' {
				cur.WriteRune(r)
			} else {
				flush()
			}
		case ';', '\n', '(', ')', '{', '}':
			flush()
		case '|':
			if i+1 < len(rs) && rs[i+1] == '|' {
				i++
				flush()
			} else {
				cur.WriteRune(r)
			}
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return parts
}

// shellFields splits a simple command into words, honoring quotes and
// backslashes. It is deliberately conservative: it does no expansion.
func shellFields(s string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' && i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '\\':
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			}
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// programOf returns the program a simple command runs, or "" if it only runs
// a built-in.
func programOf(words []string) string {
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case isAssignment(w):
			continue
		case wrappers[w]:
			// Skip the wrapper and its options (and env's assignments).
			for i+1 < len(words) && strings.HasPrefix(words[i+1], "-") {
				i++
			}
			continue
		case builtins[w]:
			return ""
		case strings.TrimLeft(w, "0123456789&<>") == "" && strings.ContainsAny(w, "<>"):
			i++ // bare redirection operator: skip it and its target
			continue
		case strings.HasPrefix(w, "<") || strings.HasPrefix(w, ">") ||
			strings.HasPrefix(w, "2>") || strings.HasPrefix(w, "&>"):
			continue
		}
		return w
	}
	return ""
}

func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	if i <= 0 {
		return false
	}
	for j, r := range w[:i] {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || j > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
