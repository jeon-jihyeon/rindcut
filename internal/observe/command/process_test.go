package command_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProtocolProcess(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--observe-process" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	switch os.Args[index+1] {
	case "echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "fail":
		fmt.Fprint(os.Stdout, `{"result":"confirmed"}`)
		fmt.Fprint(os.Stderr, "secret credential")
		os.Exit(3)
	case "overflow":
		fmt.Fprint(os.Stdout, strings.Repeat("x", command.MaxOutput+1))
	case "wait":
		time.Sleep(20 * time.Second)
	case "child":
		cmd := exec.Command(os.Args[0], "-test.run=^TestProtocolProcess$", "--", "--observe-process", "linger", os.Args[index+2])
		if err := cmd.Start(); err != nil {
			os.Exit(4)
		}
		time.Sleep(20 * time.Second)
	case "linger":
		time.Sleep(800 * time.Millisecond)
		_ = os.WriteFile(os.Args[index+2], []byte("escaped"), 0600)
	case "cwd":
		dir, _ := os.Getwd()
		fmt.Fprint(os.Stdout, dir)
	case "prepare_fifo", "verify_fifo":
		if err := protocolFIFO(os.Args[index+1], os.Args[index+2]); err != nil {
			os.Exit(2)
		}
	}
	os.Exit(0)
}

func protocolFIFO(mode, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if mode == "prepare_fifo" {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			return err
		}
		_, err := command.Prepare(ctx, os.Args[0], []string{}, []string{path})
		fmt.Fprint(os.Stdout, err)
		return nil
	}
	if err := os.WriteFile(path, []byte("acme"), 0o600); err != nil {
		return err
	}
	program, err := command.Prepare(context.Background(), os.Args[0], []string{}, []string{path})
	if err != nil {
		return err
	}
	if os.Remove(path) != nil || syscall.Mkfifo(path, 0o600) != nil {
		return errors.New("fixture_failed")
	}
	cancel()
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := command.Run(ctx, program, []byte("acme"))
	if result.Started {
		return errors.New("unexpected_start")
	}
	fmt.Fprint(os.Stdout, err)
	return nil
}

func TestFingerprintFIFOIsRejected(t *testing.T) {
	for _, mode := range []string{"prepare_fifo", "verify_fifo"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			executable, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProtocolProcess$", "--", "--observe-process", mode,
				filepath.Join(t.TempDir(), "fingerprint"))
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, string(output))
			if mode == "prepare_fifo" {
				assert.Equal(t, "program_unavailable", string(output))
			} else {
				assert.Equal(t, "program_changed", string(output))
			}
		})
	}
}

func TestRun(t *testing.T) {
	tcs := []struct {
		name    string
		timeout time.Duration
		code    string
		output  string
		started bool
	}{
		{name: "echo", timeout: 3 * time.Second, output: "acme", started: true},
		{name: "fail", timeout: 3 * time.Second, code: "exit_failed", started: true},
		{name: "overflow", timeout: 3 * time.Second, code: "output_limit", started: true},
		{name: "wait", timeout: 100 * time.Millisecond, code: "timeout", started: true},
		{name: "echo_cancelled", timeout: -time.Second, code: "timeout"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exe, err := os.Executable()
			require.NoError(t, err)
			scenario := strings.TrimSuffix(tc.name, "_cancelled")
			p, err := command.Prepare(t.Context(), exe, []string{"-test.run=^TestProtocolProcess$", "--", "--observe-process", scenario}, nil)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			r, err := command.Run(ctx, p, []byte("acme"))
			code := ""
			if err != nil {
				code = err.Error()
			}
			if tc.timeout < 0 {
				assert.Contains(t, []string{"cancelled", "timeout"}, code)
			} else {
				assert.Equal(t, tc.code, code)
			}
			assert.Equal(t, tc.output, string(r.Output))
			assert.Equal(t, tc.started, r.Started)
			assert.NotContains(t, code, "secret")
		})
	}
}

func TestPreparedFingerprint(t *testing.T) {
	tcs := []struct {
		name    string
		changed bool
	}{{name: "unchanged"}, {name: "changed", changed: true}}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exe, err := os.Executable()
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "input")
			require.NoError(t, os.WriteFile(path, []byte("v1"), 0600))
			p, err := command.Prepare(t.Context(), exe,
				[]string{"-test.run=^TestProtocolProcess$", "--", "--observe-process", "echo"}, []string{path})
			require.NoError(t, err)
			if tc.changed {
				require.NoError(t, os.WriteFile(path, []byte("v2"), 0600))
			}
			execution, err := command.Run(t.Context(), p, []byte("acme"))
			assert.Equal(t, tc.changed, err != nil)
			assert.Equal(t, !tc.changed, execution.Started)
			assert.True(t, filepath.IsAbs(p.Dir))
		})
	}
}

func TestRunKillsProcessGroup(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "escaped")
	p, err := command.Prepare(t.Context(), exe, []string{"-test.run=^TestProtocolProcess$", "--", "--observe-process", "child", path}, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = command.Run(ctx, p, nil)
	require.Error(t, err)
	assert.Equal(t, "timeout", err.Error())
	time.Sleep(time.Second)
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}
