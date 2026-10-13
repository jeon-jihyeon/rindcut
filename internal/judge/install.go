package judge

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	launchdLabel = "dev.rindcut.tick"
	systemdUnit  = "rindcut-tick"
	tickSeconds  = 300
)

// Installer keeps `rindcut tick` running every 5 minutes with the OS scheduler
type Installer struct {
	// Binary is the stable path the plugin links, so an update needs no reinstall
	Binary   string
	Home     string
	UserHome string
	GOOS     string
	UID      int
	// Exec runs launchctl or systemctl, faked in tests
	Exec func(name string, args ...string) error
}

func (in Installer) Install() error {
	switch in.GOOS {
	case "darwin":
		return in.launchd()
	case "linux":
		return in.systemd()
	default:
		return fmt.Errorf("no tick scheduler for %s, run `rindcut tick` every 5 minutes yourself", in.GOOS)
	}
}

func (in Installer) Uninstall() error {
	switch in.GOOS {
	case "darwin":
		_ = in.Exec("launchctl", "bootout", in.domain()+"/"+launchdLabel)
		return removeAll(in.plistPath())
	case "linux":
		_ = in.Exec("systemctl", "--user", "disable", "--now", systemdUnit+".timer")
		dir := in.unitDir()
		return removeAll(filepath.Join(dir, systemdUnit+".service"), filepath.Join(dir, systemdUnit+".timer"))
	default:
		return nil
	}
}

// Installed reads the schedule file, which exists only after the OS scheduler accepted it
func (in Installer) Installed() bool {
	path := in.plistPath()
	if in.GOOS == "linux" {
		path = filepath.Join(in.unitDir(), systemdUnit+".timer")
	}
	_, err := os.Stat(path)
	return err == nil
}

func (in Installer) domain() string { return fmt.Sprintf("gui/%d", in.UID) }

func (in Installer) plistPath() string {
	return filepath.Join(in.UserHome, "Library", "LaunchAgents", launchdLabel+".plist")
}

func (in Installer) unitDir() string {
	return filepath.Join(in.UserHome, ".config", "systemd", "user")
}

func (in Installer) launchd() error {
	if err := writeFile(in.plistPath(), in.Plist()); err != nil {
		return err
	}
	// bootstrap refuses a label that's already loaded, so the old one goes first
	_ = in.Exec("launchctl", "bootout", in.domain()+"/"+launchdLabel)
	if err := in.Exec("launchctl", "bootstrap", in.domain(), in.plistPath()); err != nil {
		// A file left behind would make Installed claim a schedule that never runs
		return errors.Join(err, removeAll(in.plistPath()))
	}
	return nil
}

func (in Installer) systemd() error {
	service, timer := in.Units()
	dir := in.unitDir()
	if err := writeFile(filepath.Join(dir, systemdUnit+".service"), service); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, systemdUnit+".timer"), timer); err != nil {
		return err
	}
	err := in.Exec("systemctl", "--user", "daemon-reload")
	if err == nil {
		err = in.Exec("systemctl", "--user", "enable", "--now", systemdUnit+".timer")
	}
	if err != nil {
		// A unit left behind would make Installed claim a schedule that never runs
		return errors.Join(err, removeAll(filepath.Join(dir, systemdUnit+".service"), filepath.Join(dir, systemdUnit+".timer")))
	}
	return nil
}

// Plist runs the tick at load and every 5 minutes
// launchd passes no shell environment, so the home is written in
func (in Installer) Plist() []byte {
	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>tick</string></array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>%s</string>
    <key>RINDCUT_HOME</key><string>%s</string>
  </dict>
  <key>StartInterval</key><integer>%d</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, esc(in.Binary), esc(in.UserHome), esc(in.Home), tickSeconds, esc(filepath.Join(in.Home, "tick.err.log")))
	return b.Bytes()
}

// Units are a oneshot service and a timer that catches up after sleep with Persistent
func (in Installer) Units() (service, timer []byte) {
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }
	service = fmt.Appendf(nil, `[Unit]
Description=rindcut tick: judge scheduled AI jobs

[Service]
Type=oneshot
Environment=%s
Environment=%s
ExecStart=%s tick
`, q("HOME="+in.UserHome), q("RINDCUT_HOME="+in.Home), q(in.Binary))
	timer = fmt.Appendf(nil, `[Unit]
Description=rindcut tick every 5 minutes

[Timer]
OnCalendar=*:0/5
Persistent=true

[Install]
WantedBy=timers.target
`)
	return service, timer
}

func writeFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func removeAll(paths ...string) error {
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
