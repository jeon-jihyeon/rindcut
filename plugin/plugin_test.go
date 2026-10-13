package plugin_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, v), path)
}

func TestManifests(t *testing.T) {
	t.Parallel()
	var plugin struct {
		Name, Version, Description string
	}
	readJSON(t, filepath.Join(".claude-plugin", "plugin.json"), &plugin)
	var market struct {
		Plugins []struct{ Name, Source, Description string }
	}
	readJSON(t, filepath.Join("..", ".claude-plugin", "marketplace.json"), &market)
	assert.Equal(t, "rindcut", plugin.Name)
	assert.NotEmpty(t, plugin.Version)
	require.Len(t, market.Plugins, 1)
	assert.Equal(t, "./plugin", market.Plugins[0].Source)
	assert.Equal(t, plugin.Description, market.Plugins[0].Description)
	for _, f := range []string{".mcp.json", filepath.Join("hooks", "hooks.json")} {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.Contains(t, string(b), "${CLAUDE_PLUGIN_ROOT}/bin/rindcut", f)
	}
	info, err := os.Stat(filepath.Join("bin", "rindcut"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "the launcher must be executable")
}

// The SessionEnd hook has 1.5 s, so with no cached binary the launcher must not fetch or build
func TestLauncherHookSkipsFetch(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cmd := exec.Command("/bin/sh", filepath.Join("bin", "rindcut"), "hook", "session-end")
	cmd.Env = []string{"HOME=" + home, "RINDCUT_HOME=" + filepath.Join(home, ".rindcut"), "PATH=/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(`{"session_id":"x"}`)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Less(t, time.Since(start), time.Second)
}

// A cached binary of the plugin version runs, and the stable path links to it
func TestLauncherRunsCached(t *testing.T) {
	t.Parallel()
	var plugin struct{ Version string }
	readJSON(t, filepath.Join(".claude-plugin", "plugin.json"), &plugin)
	home := filepath.Join(t.TempDir(), ".rindcut")
	bin := filepath.Join(home, "bin", "v"+plugin.Version, "rindcut")
	require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o700))
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho cached \"$@\"\n"), 0o700))
	cmd := exec.Command("/bin/sh", filepath.Join("bin", "rindcut"), "version")
	cmd.Env = []string{"HOME=" + t.TempDir(), "RINDCUT_HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, "cached version", strings.TrimSpace(string(out)))
	target, err := os.Readlink(filepath.Join(home, "bin", "rindcut"))
	require.NoError(t, err)
	assert.Equal(t, bin, target)
}
