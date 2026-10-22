package check

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Interpreters whose first argument after their flags is a script file
var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "python": true, "python3": true,
	"node": true, "ruby": true, "perl": true, "php": true, "deno": true, "bun": true,
}

// Pinned records a hash of every text script a command check runs, so the check the owner reviewed at save time can't change behind them
// A script that is missing, templated or a compiled program isn't pinned
func (s Spec) Pinned(env Env) Spec {
	if s.Kind != KindCommand {
		return s
	}
	s.Scripts = nil
	for _, word := range scriptWords(s.Run) {
		path := env.scriptPath(word)
		if sum, ok := scriptHash(path); ok {
			if s.Scripts == nil {
				s.Scripts = map[string]string{}
			}
			s.Scripts[path] = sum
		}
	}
	return s
}

// scriptHash is false for anything but a regular text file
// Compiled programs are left out because their routine upgrades aren't part of the job definition
func scriptHash(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	return scriptDigest(f)
}

func scriptDigest(reader io.Reader) (string, bool) {
	start := make([]byte, 512)
	n, err := io.ReadFull(reader, start)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", false
	}
	if bytes.IndexByte(start[:n], 0) >= 0 {
		return "", false
	}
	h := sha256.New()
	h.Write(start[:n])
	if _, err := io.Copy(h, reader); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func scriptUsesSlot(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer func() { _ = f.Close() }()
	// The tail of each read is carried over, so a variable name split across two reads still matches
	const overlap = len("RINDCUT_SLOT") - 1
	var buffer [4096 + overlap]byte
	kept := 0
	for {
		n, err := f.Read(buffer[kept:])
		n += kept
		if slotEnv.Match(buffer[:n]) {
			return true
		}
		if err != nil {
			return err != io.EOF
		}
		kept = min(n, overlap)
		copy(buffer[:kept], buffer[n-kept:n])
	}
}

// changedScripts lists each pinned script whose hash differs now, with both hashes
// The current hash is empty when the script can't be read or is no longer text
func (s Spec) changedScripts() map[string]map[string]string {
	out := map[string]map[string]string{}
	for path, saved := range s.Scripts {
		now, _ := scriptHash(path)
		if now != saved {
			out[path] = map[string]string{"saved": saved, "now": now}
		}
	}
	return out
}

// scriptWords finds the script paths a shell line runs: a command with a slash, or the first argument of a known interpreter
// A templated word is skipped because its path changes per slot
func scriptWords(line string) []string {
	var out []string
	command, interpreted := true, false
	for _, word := range shellWords(line) {
		switch {
		case len(word) == 1 && strings.ContainsAny(word, "|;&()"):
			command, interpreted = true, false
			continue
		case command && assignment(word):
			continue
		case placeholder.MatchString(word):
			interpreted = false
		case interpreted && !strings.HasPrefix(word, "-"):
			out = append(out, word)
			interpreted = false
		case interpreted:
			continue
		case command && interpreters[filepath.Base(word)]:
			interpreted = true
		case command && strings.Contains(word, "/"):
			out = append(out, word)
		}
		command = false
	}
	return out
}

// A leading NAME=value assignment leaves the next word in command position
func assignment(word string) bool {
	name, _, found := strings.Cut(word, "=")
	return found && name != "" && !strings.ContainsAny(name, "/-.")
}

// shellWords splits a line roughly like sh does: quoted text stays one word, and a newline separates commands like a semicolon
func shellWords(line string) []string {
	var out []string
	var word strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	for _, r := range line {
		switch {
		case escaped:
			word.WriteString(escapedShellRune(quote, r))
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case strings.ContainsRune(" \t", r):
			flush()
		case r == '\n':
			flush()
			out = append(out, ";")
		case strings.ContainsRune("|;&()", r):
			flush()
			out = append(out, string(r))
		default:
			word.WriteRune(r)
		}
	}
	flush()
	return out
}

func escapedShellRune(quote, r rune) string {
	if r == '\n' {
		return ""
	}
	if quote == '"' && !strings.ContainsRune("$`\"\\", r) {
		return "\\" + string(r)
	}
	return string(r)
}

func (e Env) scriptPath(word string) string {
	if word == "~" || strings.HasPrefix(word, "~/") {
		word = filepath.Join(e.UserHome, strings.TrimPrefix(word, "~"))
	}
	if !filepath.IsAbs(word) {
		word = filepath.Join(e.dir(), word)
	}
	return filepath.Clean(word)
}
