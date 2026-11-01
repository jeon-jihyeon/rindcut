package store_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreProcessHelper(t *testing.T) {
	mode := os.Getenv("RINDCUT_STORE_TEST_MODE")
	if mode == "" {
		return
	}
	home := os.Getenv("RINDCUT_STORE_TEST_HOME")
	s := store.Open(home)
	switch mode {
	case "append-many":
		fmt.Println("READY")
		_, err := io.CopyN(io.Discard, os.Stdin, 1)
		require.NoError(t, err)
		for n := range 12 {
			record := records()[1]
			record.ID = os.Getenv("RINDCUT_STORE_TEST_PREFIX") + strconv.Itoa(n)
			appendRetry(t, s, record)
			time.Sleep(time.Millisecond)
		}
	case "limited-append":
		limitedAppend(t, home, s)
	case "crash-tail", "crash-published":
		crashAppend(t, home, mode == "crash-published")
	case "crash-quarantine":
		crashQuarantine(t, home)
	case "fifo-query", "fifo-append":
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		started := time.Now()
		var err error
		if mode == "fifo-query" {
			_, err = s.Query(ctx, store.Query{})
		} else {
			_, err = s.Append(ctx, []store.Record{records()[1]})
		}
		require.ErrorIs(t, err, store.ErrCorrupt)
		require.Less(t, time.Since(started), time.Second)
		fmt.Println("FIFO_REJECTED")
	default:
		t.Fatal("unknown helper operation")
	}
}

func TestFIFOStateFiles(t *testing.T) {
	tcs := []struct{ name, path, mode string }{
		{name: "head query", path: "observe/head.json", mode: "fifo-query"},
		{name: "head append", path: "observe/head.json", mode: "fifo-append"},
		{name: "ledger query", path: "observe/ledger.jsonl", mode: "fifo-query"},
		{name: "ledger append", path: "observe/ledger.jsonl", mode: "fifo-append"},
		{name: "append lock query", path: "observe-append.lock", mode: "fifo-query"},
		{name: "append lock append", path: "observe-append.lock", mode: "fifo-append"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := privateHome(t)
			_, err := store.Open(home).Append(t.Context(), records())
			require.NoError(t, err)
			path := filepath.Join(home, tc.path)
			require.NoError(t, os.Remove(path))
			require.NoError(t, syscall.Mkfifo(path, 0o600))
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			cmd := processCommand(t, home, tc.mode, "")
			cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
			cmd.Env = append(os.Environ(), "RINDCUT_STORE_TEST_HOME="+home, "RINDCUT_STORE_TEST_MODE="+tc.mode)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, string(output))
			assert.NoError(t, ctx.Err())
			assert.Contains(t, string(output), "FIFO_REJECTED")
			info, err := os.Lstat(path)
			require.NoError(t, err)
			assert.NotZero(t, info.Mode()&os.ModeNamedPipe)
		})
	}
}

func limitedAppend(t *testing.T, home string, s *store.Store) {
	t.Helper()
	var previous syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous))
	info, err := os.Stat(filepath.Join(home, "observe", "ledger.jsonl"))
	require.NoError(t, err)
	limit := previous
	limit.Cur = uint64(info.Size()) + 64
	signal.Ignore(syscall.SIGXFSZ)
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit))
	record := records()[1]
	record.ID = "partial-write"
	record.Data = json.RawMessage(`{"value":"` + strings.Repeat("x", 2048) + `"}`)
	_, err = s.Append(context.Background(), []store.Record{record})
	require.Error(t, err)
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous))
	fmt.Println("LIMITED_WRITE_REJECTED")
}

func crashAppend(t *testing.T, home string, published bool) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(home, "observe-append.lock"), os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX))
	var previous struct {
		StoreID string `json:"store_id"`
		Offset  int64  `json:"committed_offset"`
	}
	data, err := os.ReadFile(filepath.Join(home, "observe", "head.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &previous))
	line, err := json.Marshal(records()[1])
	require.NoError(t, err)
	line = append(line, '\n')
	proposed := map[string]any{
		"store_id": previous.StoreID, "committed_offset": previous.Offset + int64(len(line)),
	}
	writeJSONFile(t, filepath.Join(home, "observe", "commit.intent"),
		map[string]any{"previous": previous, "proposed": proposed})
	ledger, err := os.OpenFile(filepath.Join(home, "observe", "ledger.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	count := len(line) / 2
	if published {
		count = len(line)
	}
	_, err = ledger.Write(line[:count])
	require.NoError(t, err)
	require.NoError(t, ledger.Sync())
	if published {
		writeJSONFile(t, filepath.Join(home, "observe", "head.json"), proposed)
	}
	fmt.Println("READY")
	_, _ = io.CopyN(io.Discard, os.Stdin, 1)
}

func crashQuarantine(t *testing.T, home string) {
	t.Helper()
	s := store.Open(home)
	unlock, err := s.AcquireControl(context.Background())
	require.NoError(t, err)
	defer unlock()
	preview, err := s.RecoveryPreview(context.Background())
	require.NoError(t, err)
	f, err := os.OpenFile(filepath.Join(home, "observe-append.lock"), os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX))
	id := "abcdef0123456789abcdef0123456789"
	writeJSONFile(t, filepath.Join(home, "observe-recovery.json"), map[string]any{
		"token": preview.Token, "previous_store_id": preview.StoreID, "new_store_id": id,
		"quarantine": "store-" + id, "stage": "planned",
	})
	root := filepath.Join(home, "observe-quarantine")
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.Rename(filepath.Join(home, "observe"), filepath.Join(root, "store-"+id)))
	fmt.Println("READY")
	_, _ = io.CopyN(io.Discard, os.Stdin, 1)
}

func TestConcurrentProcesses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home := privateHome(t)
	s := store.Open(home)
	_, err := s.Append(ctx, records())
	require.NoError(t, err)
	type child struct {
		cmd   *exec.Cmd
		input io.WriteCloser
	}
	children := make([]child, 0, 3)
	for n := range 3 {
		cmd, input := startProcess(t, home, "append-many", fmt.Sprintf("writer-%d-", n))
		children = append(children, child{cmd: cmd, input: input})
	}
	for _, child := range children {
		_, err := child.input.Write([]byte("x"))
		require.NoError(t, err)
		require.NoError(t, child.input.Close())
	}
	for range 20 {
		page := queryRetry(t, s)
		assert.True(t, page.StorePresent)
		time.Sleep(time.Millisecond)
	}
	for _, child := range children {
		require.NoError(t, child.cmd.Wait())
	}
	page, err := s.Query(ctx, store.Query{})
	require.NoError(t, err)
	assert.Len(t, page.Records, 39)
	ids := make(map[string]bool)
	for _, record := range page.Records {
		ids[record.ID] = true
	}
	assert.Len(t, ids, 39)
}

func appendRetry(t *testing.T, s *store.Store, record store.Record) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, err := s.Append(ctx, []store.Record{record})
		if errors.Is(err, store.ErrBusy) {
			continue
		}
		require.NoError(t, err)
		return
	}
}

func queryRetry(t *testing.T, s *store.Store) store.Page {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		page, err := s.Query(ctx, store.Query{})
		if errors.Is(err, store.ErrBusy) {
			continue
		}
		require.NoError(t, err)
		return page
	}
}

func TestActualPartialWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home := privateHome(t)
	s := store.Open(home)
	_, err := s.Append(ctx, records())
	require.NoError(t, err)
	before, err := os.ReadFile(filepath.Join(home, "observe", "ledger.jsonl"))
	require.NoError(t, err)
	cmd := processCommand(t, home, "limited-append", "")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.Contains(t, string(output), "LIMITED_WRITE_REJECTED")
	_, err = s.Query(ctx, store.Query{})
	assert.ErrorIs(t, err, store.ErrIncomplete)
	snapshot, err := s.Prepare(ctx)
	require.NoError(t, err)
	assert.Len(t, snapshot.Records, 5)
	after, err := os.ReadFile(filepath.Join(home, "observe", "ledger.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestProcessTerminationReplay(t *testing.T) {
	tcs := []struct {
		name  string
		mode  string
		count int
	}{
		{name: "unpublished tail", mode: "crash-tail", count: 5},
		{name: "published head", mode: "crash-published", count: 6},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			home := privateHome(t)
			s := store.Open(home)
			_, err := s.Append(ctx, records())
			require.NoError(t, err)
			cmd, input := startProcess(t, home, tc.mode, "")
			require.NoError(t, cmd.Process.Kill())
			_ = input.Close()
			require.Error(t, cmd.Wait())
			_, err = s.Read(ctx)
			assert.ErrorIs(t, err, store.ErrIncomplete)
			restored, err := s.Prepare(ctx)
			require.NoError(t, err)
			assert.Len(t, restored.Records, tc.count)
		})
	}
}

func TestRecoveryCrashQuery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home := privateHome(t)
	s := store.Open(home)
	_, err := s.Append(ctx, records())
	require.NoError(t, err)
	cmd, input := startProcess(t, home, "crash-quarantine", "")
	result := make(chan error, 1)
	go func() {
		_, queryErr := s.Query(ctx, store.Query{})
		result <- queryErr
	}()
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, cmd.Process.Kill())
	_ = input.Close()
	require.Error(t, cmd.Wait())
	select {
	case err := <-result:
		assert.ErrorIs(t, err, store.ErrRecoveryPending)
	case <-time.After(5 * time.Second):
		t.Fatal("query did not return")
	}
	before := fileHashes(t, home)
	_, err = s.Read(ctx)
	assert.ErrorIs(t, err, store.ErrRecoveryPending)
	assert.Equal(t, before, fileHashes(t, home))
	preview, err := s.RecoveryPreview(ctx)
	require.NoError(t, err)
	resultRecovery, err := s.Recover(ctx, preview.Token)
	require.NoError(t, err)
	assert.Equal(t, "abcdef0123456789abcdef0123456789", resultRecovery.NewStoreID)
	snapshot, err := s.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, resultRecovery.NewStoreID, snapshot.StoreID)
}

func processCommand(t *testing.T, home, mode, prefix string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoreProcessHelper$")
	cmd.Env = append(os.Environ(),
		"RINDCUT_STORE_TEST_HOME="+home, "RINDCUT_STORE_TEST_MODE="+mode, "RINDCUT_STORE_TEST_PREFIX="+prefix)
	return cmd
}

func startProcess(t *testing.T, home, mode, prefix string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := processCommand(t, home, mode, prefix)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	input, err := cmd.StdinPipe()
	require.NoError(t, err)
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(output).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		require.Equal(t, "READY\n", line)
	case <-time.After(10 * time.Second):
		t.Fatal("child did not become ready")
	}
	return cmd, input
}
