package judge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/lock"
)

const (
	launchdLabel = "dev.rindcut.tick"
	systemdUnit  = "rindcut-tick"
	tickSeconds  = 300
	scheduleWait = 5 * time.Second
	scheduleSize = 1 << 20
)

type Installer struct {
	Binary          string
	Home            string
	UserHome        string
	ClaudeConfigDir string
	GOOS            string
	UID             int
	Name            string
	Run             func(context.Context, string, ...string) ([]byte, error)
}

type ScheduleStatus struct {
	State   string
	Reason  string
	Changed bool
}

type scheduleSnapshot struct {
	ScheduleStatus
	loaded   bool
	disabled bool
	files    map[string][]byte
	matching bool
	present  bool
}

func (in Installer) Install() error {
	_, err := in.Ensure(context.Background())
	return err
}

func (in Installer) Inspect(ctx context.Context) ScheduleStatus {
	ctx, cancel := context.WithTimeout(ctx, scheduleWait)
	defer cancel()
	return in.runnable(in.inspect(ctx).ScheduleStatus)
}

func (in Installer) Ensure(ctx context.Context) (ScheduleStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, scheduleWait)
	defer cancel()
	release, status := in.acquire(ctx)
	if release == nil {
		return status, scheduleError(status)
	}
	defer release()
	s := in.inspect(ctx)
	s.ScheduleStatus = in.runnable(s.ScheduleStatus)
	if s.State == "ready" {
		return s.ScheduleStatus, nil
	}
	if s.State == "foreign" || s.State == "unavailable" {
		return s.ScheduleStatus, scheduleError(s.ScheduleStatus)
	}
	for path, body := range in.scheduleFiles() {
		if !bytes.Equal(s.files[path], body) {
			if err := writeFile(path, body); err != nil {
				in.restore(s.files)
				return scheduleFailure("write_failed")
			}
		}
	}
	if !in.activate(ctx, s) {
		in.restore(s.files)
		return scheduleFailure("activation_failed")
	}
	status = in.runnable(in.inspect(ctx).ScheduleStatus)
	if status.State != "ready" {
		return status, scheduleError(status)
	}
	status.Changed = true
	return status, nil
}

func (in Installer) Uninstall() error {
	ctx, cancel := context.WithTimeout(context.Background(), scheduleWait)
	defer cancel()
	release, status := in.acquire(ctx)
	if release == nil {
		return scheduleError(status)
	}
	defer release()
	s := in.inspect(ctx)
	if s.State == "foreign" || s.State == "unavailable" {
		return scheduleError(s.ScheduleStatus)
	}
	if s.loaded {
		var err error
		if in.GOOS == "darwin" {
			_, err = in.run(ctx, "launchctl", "bootout", in.target())
		} else {
			_, err = in.run(ctx, "systemctl", "--user", "disable", "--now", in.name()+".timer")
		}
		if err != nil {
			return errors.New("tick scheduler removal_failed")
		}
	}
	for path := range in.scheduleFiles() {
		if err := removeAll(path); err != nil {
			return errors.New("tick scheduler removal_failed")
		}
	}
	return nil
}

// File presence for compatibility with older status consumers
func (in Installer) Installed() bool {
	path := in.plistPath()
	if in.GOOS == "linux" {
		path = filepath.Join(in.unitDir(), in.name()+".timer")
	}
	_, err := os.Stat(path)
	return err == nil
}

func (in Installer) domain() string { return fmt.Sprintf("gui/%d", in.UID) }

func (in Installer) target() string { return in.domain() + "/" + in.name() }

func (in Installer) name() string {
	if in.Name != "" {
		return in.Name
	}
	if in.GOOS == "linux" {
		return systemdUnit
	}
	return launchdLabel
}

func (in Installer) plistPath() string {
	return filepath.Join(in.UserHome, "Library", "LaunchAgents", in.name()+".plist")
}

func (in Installer) unitDir() string {
	return filepath.Join(in.UserHome, ".config", "systemd", "user")
}

func (in Installer) scheduleFiles() map[string][]byte {
	if in.GOOS == "darwin" {
		return map[string][]byte{in.plistPath(): in.Plist()}
	}
	service, timer := in.Units()
	return map[string][]byte{
		filepath.Join(in.unitDir(), in.name()+".service"): service,
		filepath.Join(in.unitDir(), in.name()+".timer"):   timer,
	}
}

func (in Installer) acquire(ctx context.Context) (func(), ScheduleStatus) {
	if status := in.valid(); status.State != "" {
		return nil, status
	}
	dir := filepath.Dir(in.plistPath())
	if in.GOOS == "linux" {
		dir = in.unitDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, ScheduleStatus{State: "unavailable", Reason: "write_failed"}
	}
	path := filepath.Join(dir, in.name()+".lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, ScheduleStatus{State: "unavailable", Reason: "scheduler_busy"}
	}
	release, err := lock.Acquire(ctx, path, time.Second)
	if err != nil {
		return nil, ScheduleStatus{State: "unavailable", Reason: "scheduler_busy"}
	}
	return release, ScheduleStatus{}
}

func (in Installer) valid() ScheduleStatus {
	if in.GOOS != "darwin" && in.GOOS != "linux" {
		return ScheduleStatus{State: "unavailable", Reason: "unsupported_os"}
	}
	if !validScheduleName(in.name()) || !in.validPaths() || in.UID < 0 {
		return ScheduleStatus{State: "unavailable", Reason: "invalid_configuration"}
	}
	return ScheduleStatus{}
}

func validScheduleName(name string) bool {
	if len(name) == 0 || len(name) > 96 || name[0] == '.' || name[0] == '-' {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func (in Installer) validPaths() bool {
	paths := []string{in.Binary, in.Home, in.UserHome}
	if in.ClaudeConfigDir != "" {
		paths = append(paths, in.ClaudeConfigDir)
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n\r") {
			return false
		}
	}
	return true
}

func (in Installer) runnable(status ScheduleStatus) ScheduleStatus {
	if status.State == "foreign" || status.State == "unavailable" {
		return status
	}
	info, err := os.Stat(in.Binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ScheduleStatus{State: "unavailable", Reason: "binary_unavailable"}
	}
	return status
}

func (in Installer) inspect(ctx context.Context) scheduleSnapshot {
	s := scheduleSnapshot{}
	if status := in.valid(); status.State != "" {
		s.ScheduleStatus = status
		return s
	}
	s = in.readFiles()
	if s.State != "" {
		return s
	}
	if in.GOOS == "darwin" {
		s.ScheduleStatus, s.loaded, s.disabled = in.inspectLaunchd(ctx)
	} else {
		s.ScheduleStatus, s.loaded = in.inspectSystemd(ctx)
		service := filepath.Join(in.unitDir(), in.name()+".service")
		if s.present && s.files[service] == nil && !s.loaded && s.State != "unavailable" {
			s.ScheduleStatus = ScheduleStatus{State: "foreign", Reason: "owner_unknown"}
		}
	}
	if s.State == "foreign" || s.State == "unavailable" {
		return s
	}
	if !s.matching && s.loaded {
		s.ScheduleStatus = ScheduleStatus{State: "inactive", Reason: "config_drift"}
	} else if !s.loaded && s.present {
		s.ScheduleStatus = ScheduleStatus{State: "inactive", Reason: "not_loaded"}
	}
	return s
}

func (in Installer) readFiles() scheduleSnapshot {
	s := scheduleSnapshot{files: make(map[string][]byte), matching: true}
	for path, expected := range in.scheduleFiles() {
		body, err := readSchedule(path)
		if err != nil {
			s.ScheduleStatus = ScheduleStatus{State: "foreign", Reason: "owner_unknown"}
			return s
		}
		s.files[path] = body
		s.matching = s.matching && bytes.Equal(body, expected)
		s.present = s.present || body != nil
		if status := in.fileOwner(path, body); status.State != "" {
			s.ScheduleStatus = status
			return s
		}
	}
	return s
}

func (in Installer) fileOwner(path string, body []byte) ScheduleStatus {
	if body == nil || in.GOOS == "linux" && !strings.HasSuffix(path, ".service") {
		return ScheduleStatus{}
	}
	var owner string
	if in.GOOS == "darwin" {
		var label string
		owner, label = launchdOwner(body)
		if label != in.name() {
			return ScheduleStatus{State: "foreign", Reason: "target_mismatch"}
		}
	} else {
		owner = systemdOwner(body)
	}
	if owner == "" {
		return ScheduleStatus{State: "foreign", Reason: "owner_unknown"}
	}
	if owner != in.Home {
		return ScheduleStatus{State: "foreign", Reason: "home_mismatch"}
	}
	return ScheduleStatus{}
}

func (in Installer) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if in.Run == nil || ctx.Err() != nil {
		return nil, errors.New("scheduler query unavailable")
	}
	out, err := in.Run(ctx, name, args...)
	if len(out) > scheduleSize {
		return nil, errors.New("scheduler query too large")
	}
	return out, err
}

func (in Installer) activate(ctx context.Context, s scheduleSnapshot) bool {
	if in.GOOS == "darwin" {
		if s.disabled {
			if _, err := in.run(ctx, "launchctl", "enable", in.target()); err != nil {
				return false
			}
		}
		if s.loaded && s.Reason != "disabled" {
			if _, err := in.run(ctx, "launchctl", "bootout", in.target()); err != nil {
				return false
			}
		}
		if !s.loaded || s.Reason != "disabled" {
			_, err := in.run(ctx, "launchctl", "bootstrap", in.domain(), in.plistPath())
			return err == nil
		}
		return true
	}
	if _, err := in.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return false
	}
	_, err := in.run(ctx, "systemctl", "--user", "enable", "--now", in.name()+".timer")
	return err == nil
}

func (in Installer) restore(files map[string][]byte) {
	for path, body := range files {
		if body == nil {
			_ = removeAll(path)
		} else {
			_ = writeFile(path, body)
		}
	}
}

func scheduleFailure(reason string) (ScheduleStatus, error) {
	s := ScheduleStatus{State: "unavailable", Reason: reason}
	return s, scheduleError(s)
}

func scheduleError(s ScheduleStatus) error { return errors.New("tick scheduler " + s.Reason) }

func readSchedule(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > scheduleSize {
		return nil, errors.New("invalid schedule file")
	}
	body, err := io.ReadAll(io.LimitReader(f, scheduleSize+1))
	if err != nil || len(body) > scheduleSize {
		return nil, errors.New("invalid schedule file")
	}
	return body, nil
}

func sameSchedulePath(actual, expected string) bool {
	if actual == expected {
		return true
	}
	if !filepath.IsAbs(actual) {
		return false
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(expected))
	return err == nil && actual == filepath.Join(dir, filepath.Base(expected))
}

func writeFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".rindcut-schedule-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(body); err == nil {
		err = f.Chmod(0o644)
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func removeAll(paths ...string) error {
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
