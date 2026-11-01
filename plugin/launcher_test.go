package plugin_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) != "curl" || os.Getenv("RINDCUT_TEST_CURL") == "" {
		os.Exit(m.Run())
	}
	args := append([]string(nil), os.Args[1:]...)
	for i, arg := range args {
		if strings.HasPrefix(arg, "https://") &&
			!strings.HasPrefix(arg, "https://github.com/jeon-jihyeon/rindcut/releases/download/") {
			fmt.Fprintln(os.Stderr, "unexpected release URL")
			os.Exit(1)
		}
		if strings.HasPrefix(arg, "https://github.com/jeon-jihyeon/rindcut/releases/download/") {
			args[i] = os.Getenv("RINDCUT_TEST_SERVER") + strings.TrimPrefix(arg, "https://github.com")
		}
	}
	cmd := exec.Command(os.Getenv("RINDCUT_TEST_CURL"), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type launcherFixture struct {
	version, home, path, url, curl string
	requests                       []string
	mu                             sync.Mutex
}

func newLauncherFixture(t *testing.T, archive []byte, archiveStatus, checksumStatus int, checksum string) *launcherFixture {
	t.Helper()
	var plugin struct{ Version string }
	readJSON(t, filepath.Join(".claude-plugin", "plugin.json"), &plugin)
	f := &launcherFixture{version: plugin.Version, home: t.TempDir(), path: t.TempDir()}
	var err error
	f.curl, err = exec.LookPath("curl")
	require.NoError(t, err)
	for _, name := range []string{
		"dirname", "sed", "head", "readlink", "mkdir", "ln", "mv", "rm", "uname", "tr",
		"mktemp", "cut", "awk", "tar", "gzip", "chmod", "shasum",
	} {
		path, lookupErr := exec.LookPath(name)
		require.NoError(t, lookupErr)
		require.NoError(t, os.Symlink(path, filepath.Join(f.path, name)))
	}
	self, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, os.Symlink(self, filepath.Join(f.path, "curl")))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.Path)
		f.mu.Unlock()
		prefix := "/jeon-jihyeon/rindcut/releases/download/v" + f.version + "/"
		switch r.URL.Path {
		case prefix + fmt.Sprintf("rindcut_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH):
			w.WriteHeader(archiveStatus)
			_, _ = w.Write(archive)
		case prefix + "checksums.txt":
			w.WriteHeader(checksumStatus)
			_, _ = fmt.Fprint(w, checksum)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

func (f *launcherFixture) run(t *testing.T, env ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", filepath.Join("bin", "rindcut"), "status")
	cmd.Env = append([]string{
		"HOME=" + f.home, "RINDCUT_HOME=" + filepath.Join(f.home, ".rindcut"), "PATH=" + f.path,
		"RINDCUT_TEST_CURL=" + f.curl, "RINDCUT_TEST_SERVER=" + f.url, "GORACE=atexit_sleep_ms=0",
	}, env...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, ctx.Err(), string(out))
	code := 0
	if err != nil {
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit, string(out))
		code = exit.ExitCode()
	}
	return string(out), code
}

func launcherArchive(t *testing.T, body string, entries ...tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		entry.Mode = 0o755
		if entry.Typeflag == tar.TypeReg {
			entry.Size = int64(len(body))
		}
		require.NoError(t, tw.WriteHeader(&entry))
		if entry.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return b.Bytes()
}

func launcherChecksum(archive []byte) string {
	return fmt.Sprintf("%x  rindcut_%s_%s.tar.gz\n", sha256.Sum256(archive), runtime.GOOS, runtime.GOARCH)
}

func launcherBinary(t *testing.T, path, version, source string) {
	t.Helper()
	body := fmt.Sprintf("#!/bin/sh\nif [ \"${1:-}\" = version ]; then echo '%s'; else echo '%s'; fi\n", version, source)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o700))
}

func TestLauncherRelease(t *testing.T) {
	t.Parallel()
	type want struct {
		code    int
		output  string
		cached  bool
		fetched bool
	}
	type args struct {
		kind           byte
		member         string
		archiveStatus  int
		checksumStatus int
		badChecksum    bool
		brokenArchive  bool
		duplicate      bool
		missingHash    bool
		missingSum     bool
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "cold fetch", args: args{kind: tar.TypeReg, member: "rindcut", archiveStatus: 200, checksumStatus: 200},
			want: want{output: `(?m)^release$`, cached: true, fetched: true}},
		{name: "checksum mismatch stops fallback", args: args{kind: tar.TypeReg, member: "rindcut",
			archiveStatus: 200, checksumStatus: 200, badChecksum: true},
			want: want{code: 1, output: "refusing to run"}},
		{name: "unavailable archive uses path", args: args{kind: tar.TypeReg, member: "rindcut",
			archiveStatus: 404, checksumStatus: 200}, want: want{output: `(?m)^path$`}},
		{name: "unavailable checksums uses path", args: args{kind: tar.TypeReg, member: "rindcut",
			archiveStatus: 200, checksumStatus: 404}, want: want{output: `(?m)^path$`}},
		{name: "missing binary uses path", args: args{kind: tar.TypeReg, member: "other",
			archiveStatus: 200, checksumStatus: 200}, want: want{output: `(?m)^path$`}},
		{name: "directory is not cached", args: args{kind: tar.TypeDir, member: "rindcut",
			archiveStatus: 200, checksumStatus: 200}, want: want{output: `(?m)^path$`}},
		{name: "truncated archive is not cached", args: args{kind: tar.TypeReg, member: "rindcut", brokenArchive: true,
			archiveStatus: 200, checksumStatus: 200}, want: want{output: `(?m)^path$`}},
		{name: "duplicate binary is not cached", args: args{kind: tar.TypeReg, member: "rindcut", duplicate: true,
			archiveStatus: 200, checksumStatus: 200}, want: want{output: `(?m)^path$`}},
		{name: "missing hash utility uses path", args: args{kind: tar.TypeReg, member: "rindcut", missingHash: true,
			archiveStatus: 200, checksumStatus: 200}, want: want{output: `(?m)^path$`}},
		{name: "missing checksum stops fallback", args: args{kind: tar.TypeReg, member: "rindcut", missingSum: true,
			archiveStatus: 200, checksumStatus: 200}, want: want{code: 1, output: "refusing to run"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries := []tar.Header{{Name: tc.args.member, Typeflag: tc.args.kind}}
			if tc.args.duplicate {
				entries = append(entries, entries[0])
			}
			archive := launcherArchive(t, "#!/bin/sh\necho release\n", entries...)
			if tc.args.brokenArchive {
				archive = archive[:len(archive)/2]
			}
			checksum := launcherChecksum(archive)
			if tc.args.badChecksum {
				checksum = strings.Repeat("0", 64) + checksum[64:]
			}
			if tc.args.missingSum {
				checksum = ""
			}
			f := newLauncherFixture(t, archive, tc.args.archiveStatus, tc.args.checksumStatus, checksum)
			if tc.args.missingHash {
				require.NoError(t, os.Remove(filepath.Join(f.path, "shasum")))
			}
			launcherBinary(t, filepath.Join(f.path, "rindcut"), f.version, "path")
			bin := filepath.Join(f.home, ".rindcut", "bin", "v"+f.version, "rindcut")
			expectedTarget := ""
			if tc.want.code == 0 {
				expectedTarget = filepath.Join(f.path, "rindcut")
			}
			if tc.want.cached {
				expectedTarget = bin
			}
			out, code := f.run(t)
			assert.Equal(t, tc.want.code, code, out)
			assert.Regexp(t, tc.want.output, out)
			assert.Equal(t, tc.want.fetched, strings.Contains(out, "fetched v"))
			_, err := os.Stat(bin)
			assert.Equal(t, tc.want.cached, err == nil, out)
			partial, err := filepath.Glob(filepath.Join(filepath.Dir(bin), ".fetch.*"))
			require.NoError(t, err)
			assert.Empty(t, partial)
			f.mu.Lock()
			requests := append([]string(nil), f.requests...)
			f.mu.Unlock()
			assert.Contains(t, requests,
				fmt.Sprintf("/jeon-jihyeon/rindcut/releases/download/v%s/rindcut_%s_%s.tar.gz",
					f.version, runtime.GOOS, runtime.GOARCH))
			target, err := os.Readlink(filepath.Join(f.home, ".rindcut", "bin", "rindcut"))
			assert.Equal(t, tc.want.code == 0, err == nil)
			assert.Equal(t, expectedTarget, target)
		})
	}
}

func TestLauncherIgnoresOtherVersionCache(t *testing.T) {
	t.Parallel()
	archive := launcherArchive(t, "#!/bin/sh\necho release\n", tar.Header{Name: "rindcut", Typeflag: tar.TypeReg})
	f := newLauncherFixture(t, archive, 200, 200, launcherChecksum(archive))
	old := filepath.Join(f.home, ".rindcut", "bin", "v0.0.1", "rindcut")
	launcherBinary(t, old, "0.0.1", "old")
	stable := filepath.Join(f.home, ".rindcut", "bin", "rindcut")
	require.NoError(t, os.Symlink(old, stable))
	out, code := f.run(t)
	assert.Zero(t, code, out)
	assert.Contains(t, strings.Split(out, "\n"), "release")
	target, err := os.Readlink(stable)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(f.home, ".rindcut", "bin", "v"+f.version, "rindcut"), target)
}

func TestLauncherPathVersion(t *testing.T) {
	t.Parallel()
	type want struct {
		code   int
		output string
	}
	tcs := []struct {
		name, version, allow string
		want                 want
	}{
		{name: "matching version", want: want{output: "path"}},
		{name: "other version", version: "0.0.1", want: want{code: 1, output: "but the plugin needs"}},
		{name: "explicit other version", version: "0.0.1", allow: "1", want: want{output: "path"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLauncherFixture(t, nil, 404, 404, "")
			version := tc.version
			if version == "" {
				version = f.version
			}
			launcherBinary(t, filepath.Join(f.path, "rindcut"), version, "path")
			out, code := f.run(t, "RINDCUT_ALLOW_PATH="+tc.allow)
			assert.Equal(t, tc.want.code, code, out)
			assert.Contains(t, out, tc.want.output)
		})
	}
}

func TestLauncherGoInstallPinsVersion(t *testing.T) {
	t.Parallel()
	f := newLauncherFixture(t, nil, 404, 404, "")
	goBin, err := exec.LookPath("go")
	require.NoError(t, err)
	require.NoError(t, os.Symlink(goBin, filepath.Join(f.path, "go")))
	proxy := filepath.Join(t.TempDir(), "proxy")
	module := "github.com/jeon-jihyeon/rindcut"
	tag := "v" + f.version
	dir := filepath.Join(proxy, module, "@v")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	mod := "module " + module + "\n\ngo 1.25.0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, tag+".mod"), []byte(mod), 0o600))
	info := fmt.Sprintf(`{"Version":%q,"Time":"2026-01-01T00:00:00Z"}`, tag)
	require.NoError(t, os.WriteFile(filepath.Join(dir, tag+".info"), []byte(info), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "list"), []byte(tag+"\n"), 0o600))
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	for name, body := range map[string]string{
		"go.mod": mod,
		"cmd/rindcut/main.go": "package main\nimport \"fmt\"\nvar version = \"dev\"\n" +
			"func main() { fmt.Println(version) }\n",
	} {
		entry, createErr := zw.Create(module + "@" + tag + "/" + name)
		require.NoError(t, createErr)
		_, writeErr := entry.Write([]byte(body))
		require.NoError(t, writeErr)
	}
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, tag+".zip"), data.Bytes(), 0o600))
	out, code := f.run(t, "GOPROXY=file://"+proxy, "GOSUMDB=off", "GOTOOLCHAIN=local",
		"GOPATH="+filepath.Join(f.home, "go"), "GOCACHE="+filepath.Join(f.home, "cache"), "GOFLAGS=-modcacherw")
	assert.Zero(t, code, out)
	assert.True(t, strings.HasSuffix(out, f.version+"\n"), out)
	bin := filepath.Join(f.home, ".rindcut", "bin", tag, "rindcut")
	binaryInfo, err := buildinfo.ReadFile(bin)
	require.NoError(t, err)
	assert.Contains(t, binaryInfo.Settings, debug.BuildSetting{Key: "-trimpath", Value: "true"})
	target, err := os.Readlink(filepath.Join(f.home, ".rindcut", "bin", "rindcut"))
	require.NoError(t, err)
	assert.Equal(t, bin, target)
}

func TestLauncherRejectsArchiveLink(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		kind byte
	}{
		{name: "symlink", kind: tar.TypeSymlink},
		{name: "hardlink", kind: tar.TypeLink},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := filepath.Join(t.TempDir(), "outside")
			launcherBinary(t, target, "other", "outside")
			require.NoError(t, os.Chmod(target, 0o600))
			archive := launcherArchive(t, "", tar.Header{Name: "rindcut", Typeflag: tc.kind, Linkname: target})
			f := newLauncherFixture(t, archive, 200, 200, launcherChecksum(archive))
			launcherBinary(t, filepath.Join(f.path, "rindcut"), f.version, "path")
			out, code := f.run(t)
			assert.Zero(t, code, out)
			assert.Contains(t, strings.Split(out, "\n"), "path")
			assert.NotContains(t, out, "outside")
			info, err := os.Stat(target)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			_, err = os.Lstat(filepath.Join(f.home, ".rindcut", "bin", "v"+f.version, "rindcut"))
			assert.True(t, os.IsNotExist(err))
		})
	}
}
