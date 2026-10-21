package home_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/home"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want home.Dir
		bad  bool
	}{
		{name: "under HOME", env: map[string]string{"HOME": "/home/ann"}, want: "/home/ann/.rindcut"},
		{name: "RINDCUT_HOME wins", env: map[string]string{"HOME": "/home/ann", home.Env: "/data/rc/"}, want: "/data/rc"},
		{name: "RINDCUT_HOME without HOME", env: map[string]string{home.Env: "/data/rc"}, want: "/data/rc"},
		{name: "neither", env: map[string]string{}, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := home.Resolve(func(k string) string { return tt.env[k] })

			if tt.bad {
				assert.ErrorContains(t, err, home.Env)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDirPaths(t *testing.T) {
	t.Parallel()

	d := home.Dir("/h")

	assert.Equal(t, "/h/ledger.jsonl", d.Ledger())
	assert.Equal(t, "/h/tick.lock", d.Lock("tick"))
	assert.Equal(t, "/h/jobs/a.json", d.Path("jobs", "a.json"))
}

func TestEnsure(t *testing.T) {
	t.Parallel()

	d := home.Dir(filepath.Join(t.TempDir(), "a", "b"))

	require.NoError(t, d.Ensure())

	info, err := os.Stat(string(d))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}
