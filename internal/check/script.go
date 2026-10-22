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

// Interpreters whose first argument that isn't a flag is the script they run
var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "python": true, "python3": true,
	"node": true, "ruby": true, "perl": true, "php": true, "deno": true, "bun": true,
}

// Pinned records the hash of every script file a command check runs, so an edit to the script can't change what its verdicts mean
// Other kinds come back as they are
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

// scriptHash is the sha256 of a regular text file
// A compiled program has a NUL byte near its start and changes with every upgrade the owner didn't make for this job, so it isn't pinned
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
	start := make([]byte, 512)
	n, _ := io.ReadFull(f, start)
	if bytes.IndexByte(start[:n], 0) >= 0 {
		return "", false
	}
	h := sha256.New()
	h.Write(start[:n])
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// changedScripts maps each pinned script whose content differs from its pin to its saved and current hash
// A script that can't be read or is no longer text has an empty current hash
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

// scriptWords lists the words of a shell line that may name a script file
// 1. a word in command position that contains a slash, like ./check.sh
// 2. the first word that isn't a flag after an interpreter, like report.py in python3 -u report.py
// Words with a template variable change per slot, so they are skipped
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

// An assignment like LANG=C before a command leaves the next word in command position
func assignment(word string) bool {
	name, _, found := strings.Cut(word, "=")
	return found && name != "" && !strings.ContainsAny(name, "/-.")
}

// shellWords splits a line on spaces and the separators | ; & ( ), which come back as their own words
// Quotes are dropped and the text inside them stays one word, which is enough to find script paths
func shellWords(line string) []string {
	var out []string
	var word strings.Builder
	var quote rune
	flush := func() {
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	for _, r := range line {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
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

// A script path resolves like the shell resolves it, against the folder the command runs in
func (e Env) scriptPath(word string) string {
	if word == "~" || strings.HasPrefix(word, "~/") {
		word = filepath.Join(e.UserHome, strings.TrimPrefix(word, "~"))
	}
	if !filepath.IsAbs(word) {
		word = filepath.Join(e.dir(), word)
	}
	return filepath.Clean(word)
}
