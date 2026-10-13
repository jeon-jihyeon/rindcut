// Package home resolves the one directory every rindcut file lives under
package home

import (
	"errors"
	"os"
	"path/filepath"
)

// Env names the variable that moves the home, used by tests and by owners who want it elsewhere
const Env = "RINDCUT_HOME"

// Dir is the rindcut home, `~/.rindcut` unless RINDCUT_HOME is set
type Dir string

func Resolve(getenv func(string) string) (Dir, error) {
	if h := getenv(Env); h != "" {
		return Dir(filepath.Clean(h)), nil
	}
	user := getenv("HOME")
	if user == "" {
		return "", errors.New("HOME is not set, set " + Env + " to choose a home")
	}
	return Dir(filepath.Join(user, ".rindcut")), nil
}

func (d Dir) Path(elem ...string) string {
	return filepath.Join(append([]string{string(d)}, elem...)...)
}

// Ensure creates the home with owner-only access, since the ledger holds check output and exporter keys
func (d Dir) Ensure() error {
	return os.MkdirAll(string(d), 0o700)
}

func (d Dir) Ledger() string       { return d.Path("ledger.jsonl") }
func (d Dir) Lock(n string) string { return d.Path(n + ".lock") }
