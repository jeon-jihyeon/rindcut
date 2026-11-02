package judge_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/judge"
	"github.com/jeon-jihyeon/rindcut/internal/lock"
)

var schedulerMissing = sync.OnceValue(func() error {
	command := exec.Command(os.Args[0], "-test.run=^TestScheduleAbsentProcess$")
	command.Env = append(os.Environ(), "RINDCUT_SCHEDULE_ABSENT_TEST=1")
	return command.Run()
})

func TestScheduleAbsentProcess(t *testing.T) {
	if os.Getenv("RINDCUT_SCHEDULE_ABSENT_TEST") == "1" {
		os.Exit(113)
	}
}

type scheduleFixture struct {
	in       judge.Installer
	loaded   bool
	disabled bool
	failOn   string
	raw      func(string, string) string
	actions  []string
}

func newScheduleFixture(t *testing.T, goos string) *scheduleFixture {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	user := t.TempDir()
	f := &scheduleFixture{in: judge.Installer{Binary: binary, Home: filepath.Join(user, "data"),
		UserHome: user, GOOS: goos, UID: 501}}
	f.in.Run = f.run
	return f
}

func (f *scheduleFixture) run(_ context.Context, name string, args ...string) ([]byte, error) {
	command := name + " " + strings.Join(args, " ")
	if f.failOn != "" && strings.Contains(command, f.failOn) {
		f.actions = append(f.actions, command)
		return nil, errors.New("private OS failure detail")
	}
	var out string
	switch {
	case name == "launchctl" && args[0] == "print-disabled":
		out = fmt.Sprintf("disabled services = {\n\t%q => %t\n}\n", f.name(), f.disabled)
	case name == "launchctl" && args[0] == "print":
		if !f.loaded {
			return nil, schedulerMissing()
		}
		out = fmt.Sprintf(`gui/501/%s = {
	path = %s
	state = not running
	run interval = 300 seconds
	program = %s
	arguments = {
		%s
		tick
	}
	environment = {
		HOME => %s
		RINDCUT_HOME => %s
`, f.name(), f.path(".plist"), f.in.Binary, f.in.Binary, f.in.UserHome, f.in.Home)
		if f.in.ClaudeConfigDir != "" {
			out += "\t\tCLAUDE_CONFIG_DIR => " + f.in.ClaudeConfigDir + "\n"
		}
		out += "\t}\n}\n"
	case name == "systemctl" && args[1] == "show":
		switch {
		case !f.loaded:
			if strings.HasSuffix(args[2], ".timer") {
				out = "LoadState=not-found\nActiveState=\nUnitFileState=\nFragmentPath=\nDropInPaths=\n" +
					"Unit=\nTimersCalendar=\nTimersMonotonic=\nPersistent=\n"
			} else {
				out = "LoadState=not-found\nFragmentPath=\nDropInPaths=\nExecStart=\nEnvironment=\n"
			}
		case strings.HasSuffix(args[2], ".timer"):
			out = "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\nFragmentPath=" +
				f.path(".timer") + "\nDropInPaths=\nUnit=" + f.name() + ".service\n" +
				"TimersCalendar={ OnCalendar=*-*-* *:00/5:00 ; next_elapse=Mon 2026-11-02 12:00:00 UTC }\n" +
				"TimersMonotonic=\nPersistent=yes\n"
		default:
			out = "LoadState=loaded\nFragmentPath=" + f.path(".service") + "\nDropInPaths=\nExecStart={ path=" +
				f.in.Binary + " ; argv[]=" + f.in.Binary + " tick ; ignore_errors=no ; }\nEnvironment=" +
				strconv.Quote("HOME="+f.in.UserHome) + " " + strconv.Quote("RINDCUT_HOME="+f.in.Home)
			if f.in.ClaudeConfigDir != "" {
				out += " " + strconv.Quote("CLAUDE_CONFIG_DIR="+f.in.ClaudeConfigDir)
			}
			out += "\n"
		}
	default:
		f.actions = append(f.actions, command)
		switch {
		case name == "launchctl" && args[0] == "enable":
			f.disabled = false
		case name == "launchctl" && args[0] == "bootstrap":
			f.loaded, f.raw = true, nil
		case name == "launchctl" && args[0] == "bootout":
			f.loaded = false
		case name == "systemctl" && args[1] == "enable":
			f.loaded, f.raw = true, nil
		case name == "systemctl" && args[1] == "disable":
			f.loaded = false
		}
	}
	if f.raw != nil {
		out = f.raw(command, out)
	}
	return []byte(out), nil
}

func (f *scheduleFixture) name() string {
	if f.in.Name != "" {
		return f.in.Name
	}
	if f.in.GOOS == "linux" {
		return "rindcut-tick"
	}
	return "dev.rindcut.tick"
}

func (f *scheduleFixture) path(suffix string) string {
	if suffix == ".plist" {
		return filepath.Join(f.in.UserHome, "Library", "LaunchAgents", f.name()+suffix)
	}
	return filepath.Join(f.in.UserHome, ".config", "systemd", "user", f.name()+suffix)
}

func (f *scheduleFixture) write(t *testing.T) {
	t.Helper()
	files := map[string][]byte{f.path(".plist"): f.in.Plist()}
	if f.in.GOOS == "linux" {
		service, timer := f.in.Units()
		files = map[string][]byte{f.path(".service"): service, f.path(".timer"): timer}
	}
	for path, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, body, 0o644))
	}
}

func TestInstallPreservesForeignHome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, goos string }{
		{name: "launchd", goos: "darwin"},
		{name: "systemd", goos: "linux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			user := t.TempDir()
			calls := 0
			binary, err := os.Executable()
			require.NoError(t, err)
			in := judge.Installer{Binary: binary, Home: filepath.Join(user, "current"),
				UserHome: user, GOOS: tc.goos, UID: 501,
				Run: func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, nil }}
			foreign := in
			foreign.Home = filepath.Join(user, "other")
			files := map[string][]byte{}
			if tc.goos == "darwin" {
				files[filepath.Join(user, "Library", "LaunchAgents", "dev.rindcut.tick.plist")] = foreign.Plist()
			} else {
				service, timer := foreign.Units()
				files[filepath.Join(user, ".config", "systemd", "user", "rindcut-tick.service")] = service
				files[filepath.Join(user, ".config", "systemd", "user", "rindcut-tick.timer")] = timer
			}
			for path, body := range files {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, body, 0o644))
			}
			err = in.Install()
			assert.EqualError(t, err, "tick scheduler home_mismatch")
			assert.Equal(t, judge.ScheduleStatus{State: "foreign", Reason: "home_mismatch"},
				in.Inspect(context.Background()))
			assert.Zero(t, calls)
			for path, body := range files {
				got, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Equal(t, body, got)
			}
		})
	}
}

func TestScheduleLifecycle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want int
	}{
		{name: "darwin", want: 1},
		{name: "linux", want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newScheduleFixture(t, tc.name)
			assert.Equal(t, judge.ScheduleStatus{State: "absent", Reason: "not_registered"}, f.in.Inspect(context.Background()))
			s, err := f.in.Ensure(context.Background())
			require.NoError(t, err)
			assert.Equal(t, judge.ScheduleStatus{State: "ready", Reason: "registered", Changed: true}, s)
			assert.True(t, f.in.Installed())
			first := append([]string(nil), f.actions...)
			assert.Len(t, first, tc.want, "fresh enabled registration must not create a disabled override")
			s, err = f.in.Ensure(context.Background())
			require.NoError(t, err)
			assert.Equal(t, judge.ScheduleStatus{State: "ready", Reason: "registered"}, s)
			assert.Equal(t, first, f.actions, "a healthy scheduler must not be restarted")
			require.NoError(t, f.in.Uninstall())
			assert.False(t, f.in.Installed())
			assert.Equal(t, judge.ScheduleStatus{State: "absent", Reason: "not_registered"}, f.in.Inspect(context.Background()))
			require.NoError(t, f.in.Uninstall())
		})
	}
}

func TestScheduleInspection(t *testing.T) {
	t.Parallel()
	type args struct {
		goos, before, after string
		missing, disabled   bool
	}
	tcs := []struct {
		name string
		args args
		want judge.ScheduleStatus
	}{
		{
			name: "oneshot waiting is healthy",
			args: args{
				goos: "darwin",
			},
			want: judge.ScheduleStatus{State: "ready", Reason: "registered"},
		},
		{
			name: "launchd proxy spawning is registered",
			args: args{goos: "darwin", before: "state = not running", after: "state = xpcproxy"},
			want: judge.ScheduleStatus{State: "ready", Reason: "registered"},
		},
		{
			name: "disabled registration",
			args: args{
				goos:     "darwin",
				disabled: true,
			},
			want: judge.ScheduleStatus{State: "inactive", Reason: "disabled"},
		},
		{
			name: "missing loaded plist",
			args: args{
				goos:    "darwin",
				missing: true,
			},
			want: judge.ScheduleStatus{State: "inactive", Reason: "config_drift"},
		},
		{
			name: "foreign loaded HOME",
			args: args{
				goos:    "darwin",
				missing: true,
				before:  "RINDCUT_HOME => ",
				after:   "RINDCUT_HOME => /foreign",
			},
			want: judge.ScheduleStatus{State: "foreign", Reason: "home_mismatch"},
		},
		{
			name: "foreign path",
			args: args{
				goos:   "darwin",
				before: "path = ",
				after:  "path = /foreign",
			},
			want: judge.ScheduleStatus{State: "foreign", Reason: "target_mismatch"},
		},
		{
			name: "wrong program",
			args: args{
				goos:   "darwin",
				before: "program = ",
				after:  "program = /wrong",
			},
			want: judge.ScheduleStatus{State: "inactive", Reason: "config_drift"},
		},
		{
			name: "unknown launchctl state",
			args: args{
				goos:   "darwin",
				before: "state = not running",
				after:  "state = future-state",
			},
			want: judge.ScheduleStatus{State: "unavailable", Reason: "query_format"},
		},
		{
			name: "stale launchd interval",
			args: args{
				goos:   "darwin",
				before: "run interval = 300 seconds",
				after:  "run interval = 600 seconds",
			},
			want: judge.ScheduleStatus{State: "inactive", Reason: "config_drift"},
		},
		{
			name: "unknown launchctl format",
			args: args{
				goos:   "darwin",
				before: " = {",
				after:  ": [",
			},
			want: judge.ScheduleStatus{State: "unavailable", Reason: "query_format"},
		},
		{
			name: "duplicate loaded owner",
			args: args{
				goos:   "darwin",
				before: "\t\tHOME => ",
				after:  "\t\tRINDCUT_HOME => ",
			},
			want: judge.ScheduleStatus{State: "unavailable", Reason: "query_format"},
		},
		{
			name: "systemd timer healthy",
			args: args{
				goos: "linux",
			},
			want: judge.ScheduleStatus{State: "ready", Reason: "registered"},
		},
		{
			name: "inactive timer",
			args: args{
				goos:   "linux",
				before: "ActiveState=active",
				after:  "ActiveState=inactive",
			},
			want: judge.ScheduleStatus{State: "inactive", Reason: "timer_inactive"},
		},
		{
			name: "disabled timer",
			args: args{
				goos:   "linux",
				before: "UnitFileState=enabled",
				after:  "UnitFileState=disabled",
			},
			want: judge.ScheduleStatus{State: "inactive", Reason: "not_enabled"},
		},
		{
			name: "stale systemd interval",
			args: args{
				goos:   "linux",
				before: "*:00/5:00",
				after:  "*:00/10:00",
			},
			want: judge.ScheduleStatus{State: "inactive", Reason: "config_drift"},
		},
		{
			name: "different triggered service",
			args: args{
				goos:   "linux",
				before: "Unit=rindcut-tick.service",
				after:  "Unit=foreign.service",
			},
			want: judge.ScheduleStatus{State: "foreign", Reason: "target_mismatch"},
		},
		{
			name: "foreign loaded unit HOME",
			args: args{
				goos:    "linux",
				missing: true,
				before:  "RINDCUT_HOME=",
				after:   "RINDCUT_HOME=/foreign",
			},
			want: judge.ScheduleStatus{State: "foreign", Reason: "home_mismatch"},
		},
		{
			name: "foreign dropin",
			args: args{
				goos:   "linux",
				before: "DropInPaths=",
				after:  "DropInPaths=/foreign.conf",
			},
			want: judge.ScheduleStatus{State: "foreign", Reason: "target_mismatch"},
		},
		{
			name: "unknown systemd format",
			args: args{
				goos:   "linux",
				before: "LoadState=",
				after:  "Unknown=",
			},
			want: judge.ScheduleStatus{State: "unavailable", Reason: "query_format"},
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newScheduleFixture(t, tc.args.goos)
			f.loaded, f.disabled = true, tc.args.disabled
			if !tc.args.missing {
				f.write(t)
			}
			if tc.args.before != "" {
				f.raw = func(_ string, out string) string { return strings.ReplaceAll(out, tc.args.before, tc.args.after) }
			}
			assert.Equal(t, tc.want, f.in.Inspect(context.Background()))
			assert.Empty(t, f.actions)
			if tc.want.State == "foreign" || tc.want.State == "unavailable" {
				_, err := f.in.Ensure(context.Background())
				assert.Error(t, err)
				assert.NotContains(t, err.Error(), "private OS failure detail")
				assert.Empty(t, f.actions)
			}
		})
	}
}

func TestScheduleRecovery(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"darwin", "linux"} {
		for _, mode := range []string{"not loaded", "disabled", "config drift", "missing files"} {
			t.Run(goos+" "+mode, func(t *testing.T) {
				t.Parallel()
				f := newScheduleFixture(t, goos)
				f.loaded = mode != "not loaded"
				f.disabled = mode == "disabled"
				if mode != "missing files" {
					f.write(t)
				}
				if mode == "disabled" && goos == "linux" {
					f.raw = func(_ string, out string) string {
						return strings.ReplaceAll(out, "UnitFileState=enabled", "UnitFileState=disabled")
					}
				}
				if mode == "config drift" {
					f.raw = func(_ string, out string) string {
						return strings.ReplaceAll(out, f.in.Binary, "/old/rindcut")
					}
				}
				s, err := f.in.Ensure(context.Background())
				require.NoError(t, err)
				assert.Equal(t, judge.ScheduleStatus{State: "ready", Reason: "registered", Changed: true}, s)
				assert.True(t, f.in.Installed())
				assert.NotEmpty(t, f.actions)
			})
		}
	}
}

func TestScheduleForeignUninstall(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			f := newScheduleFixture(t, goos)
			f.write(t)
			f.in.Home += "-other"
			err := f.in.Uninstall()
			assert.EqualError(t, err, "tick scheduler home_mismatch")
			assert.Empty(t, f.actions)
			assert.True(t, f.in.Installed())
		})
	}
}

func TestScheduleRemovalUnavailableBinary(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"missing", "not executable"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newScheduleFixture(t, "darwin")
			f.in.Binary = filepath.Join(f.in.UserHome, "rindcut")
			require.NoError(t, os.WriteFile(f.in.Binary, []byte("fixture"), 0o755))
			f.write(t)
			f.loaded = true
			if mode == "missing" {
				require.NoError(t, os.Remove(f.in.Binary))
			} else {
				require.NoError(t, os.Chmod(f.in.Binary, 0o600))
			}
			err := f.in.Uninstall()
			assert.NoError(t, err)
			assert.False(t, f.in.Installed())
			assert.Len(t, f.actions, 1)
		})
	}
}

func TestScheduleSystemdForeignTimerWithServiceDrift(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before, after string }{
		{name: "triggered unit", before: "Unit=rindcut-tick.service", after: "Unit=foreign.service"},
		{name: "dropin", before: "DropInPaths=", after: "DropInPaths=/foreign.conf"},
		{name: "fragment", before: "FragmentPath=", after: "FragmentPath=/foreign"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newScheduleFixture(t, "linux")
			f.write(t)
			f.loaded = true
			f.raw = func(command, out string) string {
				out = strings.ReplaceAll(out, "ExecStart={ path="+f.in.Binary, "ExecStart={ path=/wrong")
				if strings.Contains(command, ".timer") {
					out = strings.ReplaceAll(out, tc.before, tc.after)
				}
				return out
			}
			s, err := f.in.Ensure(context.Background())
			assert.EqualError(t, err, "tick scheduler target_mismatch")
			assert.Equal(t, judge.ScheduleStatus{State: "foreign", Reason: "target_mismatch"}, s)
			assert.Empty(t, f.actions)
		})
	}
}

func TestSchedulePreservesUnexpectedPlistLabel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, label string }{
		{name: "different", label: "dev.example.foreign"},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newScheduleFixture(t, "darwin")
			f.write(t)
			path := f.path(".plist")
			body := strings.ReplaceAll(string(f.in.Plist()), "dev.rindcut.tick", tc.label)
			require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
			s, err := f.in.Ensure(context.Background())
			assert.EqualError(t, err, "tick scheduler target_mismatch")
			assert.Equal(t, judge.ScheduleStatus{State: "foreign", Reason: "target_mismatch"}, s)
			assert.Empty(t, f.actions)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, body, string(got))
		})
	}
}

func TestScheduleBoundedAndSafe(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"query failed", "large output", "shared lock", "deadline", "symlink", "binary", "name"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newScheduleFixture(t, "darwin")
			want := judge.ScheduleStatus{State: "unavailable", Reason: "query_failed"}
			ctx := context.Background()
			switch mode {
			case "query failed":
				f.in.Run = func(context.Context, string, ...string) ([]byte, error) {
					return nil, errors.New("private OS failure detail")
				}
			case "large output":
				f.in.Run = func(context.Context, string, ...string) ([]byte, error) {
					return make([]byte, 1<<20+1), nil
				}
			case "shared lock":
				dir := filepath.Dir(f.path(".plist"))
				require.NoError(t, os.MkdirAll(dir, 0o755))
				release, err := lock.Acquire(ctx, filepath.Join(dir, "dev.rindcut.tick.lock"), 0)
				require.NoError(t, err)
				defer release()
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
				want.Reason = "scheduler_busy"
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
				f.in.Run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
					_, ok := ctx.Deadline()
					assert.True(t, ok)
					<-ctx.Done()
					return nil, ctx.Err()
				}
			case "symlink":
				target := filepath.Join(f.in.UserHome, "foreign.plist")
				require.NoError(t, os.WriteFile(target, f.in.Plist(), 0o644))
				require.NoError(t, os.MkdirAll(filepath.Dir(f.path(".plist")), 0o755))
				require.NoError(t, os.Symlink(target, f.path(".plist")))
				want = judge.ScheduleStatus{State: "foreign", Reason: "owner_unknown"}
			case "binary":
				f.in.Binary = filepath.Join(f.in.UserHome, "missing")
				want.Reason = "binary_unavailable"
			case "name":
				f.in.Name = "../foreign"
				want.Reason = "invalid_configuration"
			}
			start := time.Now()
			s, err := f.in.Ensure(ctx)
			assert.Error(t, err)
			assert.Equal(t, want, s)
			assert.NotContains(t, err.Error(), "private OS failure detail")
			assert.Less(t, time.Since(start), time.Second)
		})
	}
}

func TestScheduleLaunchdIntegration(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd integration requires macOS")
	}
	uid := os.Getuid()
	domain := fmt.Sprintf("gui/%d", uid)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "launchctl", "print-disabled", domain).Run(); err != nil {
		t.Skip("current GUI domain unavailable")
	}
	user := t.TempDir()
	name := fmt.Sprintf("dev.rindcut.verify.%d.%d", os.Getpid(), time.Now().UnixNano())
	t.Logf("unique fixture label: %s", name)
	var actions []string
	var shape []string
	in := judge.Installer{Binary: "/usr/bin/true", Home: filepath.Join(user, "data"), UserHome: user,
		GOOS: "darwin", UID: uid, Name: name,
		Run: func(ctx context.Context, command string, args ...string) ([]byte, error) {
			if args[0] != "print" && args[0] != "print-disabled" {
				actions = append(actions, strings.Join(args, " "))
			}
			out, err := exec.CommandContext(ctx, command, args...).Output()
			if args[0] == "print" && err == nil {
				shape = nil
				for line := range strings.SplitSeq(string(out), "\n") {
					line = strings.TrimSpace(line)
					if line == "}" || strings.HasSuffix(line, " = {") || strings.HasSuffix(line, " => {") ||
						strings.HasPrefix(line, "state = ") {
						shape = append(shape, strings.ReplaceAll(line, name, "fixture"))
					}
					if strings.Contains(strings.ToLower(line), "interval") {
						t.Logf("fixture interval property: %s", strings.TrimSpace(line))
					}
					if value, found := strings.CutPrefix(strings.TrimSpace(line), "path = "); found {
						resolved, resolveErr := filepath.EvalSymlinks(filepath.Join(user, "Library", "LaunchAgents", name+".plist"))
						t.Logf("manager path matches canonical fixture path: %t", resolveErr == nil && resolved == value)
					}
				}
			}
			return out, err
		}}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = exec.CommandContext(cleanup, "launchctl", "bootout", domain+"/"+name).Run()
		var exit *exec.ExitError
		err := exec.CommandContext(cleanup, "launchctl", "print", domain+"/"+name).Run()
		assert.True(t, errors.As(err, &exit) && exit.ExitCode() == 113, "unique fixture cleanup")
	})
	s, err := in.Ensure(context.Background())
	if err != nil {
		t.Logf("safe manager structure: %q", shape)
	}
	require.NoError(t, err)
	assert.Equal(t, judge.ScheduleStatus{State: "ready", Reason: "registered", Changed: true}, s)
	first := append([]string(nil), actions...)
	_, err = in.Ensure(context.Background())
	require.NoError(t, err)
	assert.Equal(t, first, actions)
	require.NoError(t, in.Uninstall())
	assert.Equal(t, judge.ScheduleStatus{State: "absent", Reason: "not_registered"}, in.Inspect(context.Background()))
	t.Logf("unique label %s registered and removed with %d mutations", name, len(actions))
}
