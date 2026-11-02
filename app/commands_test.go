package app_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/app"
)

func TestNewClaudeConfigDir(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	tcs := []struct {
		name, config, want string
	}{
		{name: "default"},
		{name: "absolute", config: "/home/ann/claude alt", want: "/home/ann/claude alt"},
		{name: "relative", config: "claude-alt", want: filepath.Join(cwd, "claude-alt")},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vars := map[string]string{"HOME": t.TempDir(), "RINDCUT_HOME": t.TempDir(), "CLAUDE_CONFIG_DIR": tc.config}
			a, err := app.New(func(k string) string { return vars[k] }, time.Now, "test", io.Discard)
			require.NoError(t, err)
			assert.Equal(t, tc.want, a.ClaudeConfigDir)
			assert.Equal(t, tc.want, a.Installer.ClaudeConfigDir)
		})
	}
}
