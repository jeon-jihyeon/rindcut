package judge_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/job"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

const criticResponse = `{"protocol":"rindcut.critic/v1","job_id":"report","job_revision":1,` +
	`"slot":"2026-10-08T09:00:00Z",` +
	`"criteria_revision":"C1","complete":true,"criteria":[{"id":"accuracy","verdict":"approved"},` +
	`{"id":"scope","verdict":"approved"}],"reason":"acme note"}`

func criticJob(t *testing.T, f *fixture, response, command string) job.Job {
	t.Helper()
	r := baseJob(f.folder)
	r.ID, r.Name, r.Evidence, r.Marker = "report", "Report", job.EvidenceNone, ""
	r.Checks = []check.Spec{{Kind: check.KindCommand, Run: "printf 'acme objective evidence'"}}
	r.Critic = &job.Critic{Source: "acme", SourceRevision: "adapter-v1", CriteriaRevision: "C1",
		Criteria: []job.Criterion{{ID: "accuracy", Text: "Numbers have sources"}, {ID: "scope", Text: "Scope is preserved"}},
		Command:  check.Spec{Kind: check.KindCommand, Run: "sh critic.sh"}}
	require.NoError(t, os.WriteFile(filepath.Join(f.folder, "response.json"), []byte(response), 0o600))
	script := "cat > input.json\nprintf x >> calls\n" + command + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(f.folder, "critic.sh"), []byte(script), 0o700))
	f.now = time.Date(2026, 10, 8, 9, 31, 0, 0, time.UTC)
	return r
}

func TestCriticProtocol(t *testing.T) {
	t.Parallel()
	type args struct {
		response string
		retain   bool
	}
	type want struct {
		verdict string
		cause   string
		reason  string
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "approved without explanation retention", want: want{verdict: "approved"}},
		{name: "explanation retained explicitly", args: args{retain: true},
			want: want{verdict: "approved", reason: "acme note"}},
		{name: "revision", args: args{response: strings.Replace(criticResponse, "approved", "revision", 1)},
			want: want{verdict: "revision"}},
		{name: "rejected dominates revision", args: args{response: strings.Replace(
			strings.Replace(criticResponse, "approved", "revision", 1), "approved", "rejected", 1)},
			want: want{verdict: "rejected"}},
		{name: "revision cannot weaken rejection", args: args{response: strings.Replace(
			strings.Replace(criticResponse, "approved", "rejected", 1), "approved", "revision", 1)},
			want: want{verdict: "rejected"}},
		{name: "uncertainty dominates rejection", args: args{response: strings.Replace(
			strings.Replace(criticResponse, "approved", "rejected", 1), "approved", "inconclusive", 1)},
			want: want{verdict: "inconclusive"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			response := tc.args.response
			if response == "" {
				response = criticResponse
			}
			r := criticJob(t, f, response, "printf 'acme raw stderr' >&2; cat response.json")
			r.Critic.RetainReason = tc.args.retain
			r = f.j.Pin(r)
			e, err := f.j.Probe(context.Background(), r, f.now, "preview")
			require.NoError(t, err)
			var p ledger.Judgment
			require.NoError(t, e.Decode(&p))
			require.NotNil(t, p.Critic)
			assert.Equal(t, ledger.VerdictPass, p.Verdict)
			assert.Equal(t, tc.want, want{verdict: string(p.Critic.Verdict), cause: p.Critic.Cause, reason: p.Critic.Reason})
			assert.Equal(t, "acme", p.Critic.Source)
			assert.Equal(t, "adapter-v1", p.Critic.SourceRevision)
			assert.Equal(t, "C1", p.Critic.CriteriaRevision)
			assert.Equal(t, f.now, p.Critic.EvaluatedAt)
			b, err := json.Marshal(p.Critic)
			require.NoError(t, err)
			assert.NotContains(t, string(b), "raw stderr")
			assert.NotContains(t, string(b), "critic.sh")
			sum := sha256.Sum256([]byte(response))
			assert.Equal(t, hex.EncodeToString(sum[:]), p.Critic.OutputSHA256)
			input, err := os.ReadFile(filepath.Join(f.folder, "input.json"))
			require.NoError(t, err)
			var received map[string]any
			require.NoError(t, json.Unmarshal(input, &received))
			assert.Len(t, received, 6)
			assert.Equal(t, "report", received["job_id"])
			assert.Equal(t, float64(1), received["job_revision"])
			assert.Len(t, received["criteria"], 2)
			assert.NotContains(t, string(input), "objective evidence")
		})
	}
}

func TestCriticProtocolErrors(t *testing.T) {
	t.Parallel()
	type args struct {
		response, command string
		changed           bool
		timeout           check.Duration
	}
	type want struct {
		verdict string
		cause   string
		reason  string
	}
	tcs := []struct {
		name string
		args args
		want want
	}{
		{name: "partial evaluation", args: args{response: strings.Replace(criticResponse, "true", "false", 1)},
			want: want{verdict: "inconclusive", cause: "incomplete_response"}},
		{name: "missing criterion", args: args{response: strings.Replace(criticResponse,
			`,{"id":"scope","verdict":"approved"}`, "", 1)},
			want: want{verdict: "inconclusive", cause: "criteria_mismatch"}},
		{name: "duplicate criterion", args: args{response: strings.Replace(criticResponse, `"scope"`, `"accuracy"`, 1)},
			want: want{verdict: "inconclusive", cause: "criteria_mismatch"}},
		{name: "extra criterion", args: args{response: strings.Replace(criticResponse, `"scope"`, `"unknown"`, 1)},
			want: want{verdict: "inconclusive", cause: "criteria_mismatch"}},
		{name: "unknown verdict", args: args{response: strings.Replace(criticResponse, "approved", "pass", 1)},
			want: want{verdict: "inconclusive", cause: "criteria_mismatch"}},
		{name: "stale criteria", args: args{response: strings.Replace(criticResponse, "C1", "C0", 1)},
			want: want{verdict: "inconclusive", cause: "criteria_revision_mismatch"}},
		{name: "other job", args: args{response: strings.Replace(criticResponse, `"job_id":"report"`,
			`"job_id":"other"`, 1)}, want: want{verdict: "inconclusive", cause: "job_revision_mismatch"}},
		{name: "other job revision", args: args{response: strings.Replace(criticResponse, `"job_revision":1`,
			`"job_revision":2`, 1)}, want: want{verdict: "inconclusive", cause: "job_revision_mismatch"}},
		{name: "other slot", args: args{response: strings.Replace(criticResponse, "09:00:00", "08:00:00", 1)},
			want: want{verdict: "inconclusive", cause: "slot_mismatch"}},
		{name: "other protocol", args: args{response: strings.Replace(criticResponse, "v1", "v2", 1)},
			want: want{verdict: "inconclusive", cause: "protocol_mismatch"}},
		{name: "trailing object", args: args{response: criticResponse + "{}"},
			want: want{verdict: "inconclusive", cause: "invalid_response"}},
		{name: "unknown field", args: args{response: strings.Replace(criticResponse, `"reason":`, `"instruction":`, 1)},
			want: want{verdict: "inconclusive", cause: "invalid_response"}},
		{name: "duplicate field", args: args{response: strings.Replace(criticResponse,
			`"complete":true`, `"complete":false,"complete":true`, 1)},
			want: want{verdict: "inconclusive", cause: "invalid_response"}},
		{name: "nested duplicate", args: args{response: strings.Replace(criticResponse,
			`"id":"scope"`, `"id":"accuracy","id":"scope"`, 1)},
			want: want{verdict: "inconclusive", cause: "invalid_response"}},
		{name: "malformed", args: args{response: "{acme"},
			want: want{verdict: "inconclusive", cause: "invalid_response"}},
		{name: "invalid UTF8 cannot disappear", args: args{response: criticResponse + string([]byte{0xff})},
			want: want{verdict: "inconclusive", cause: "invalid_response"}},
		{name: "case alias cannot replace a field", args: args{response: strings.Replace(criticResponse,
			`"complete":true`, `"complete":false,"Complete":true`, 1)},
			want: want{verdict: "inconclusive", cause: "invalid_response"}},
		{name: "truncated output", args: args{response: criticResponse + strings.Repeat(" ", 5000)},
			want: want{verdict: "inconclusive", cause: "truncated_output"}},
		{name: "nonzero exit", args: args{command: "cat response.json; exit 1"},
			want: want{verdict: "inconclusive", cause: "command_error"}},
		{name: "temporary failure", args: args{command: "printf 'acme unavailable' >&2; exit 75"},
			want: want{verdict: "inconclusive", cause: "command_error"}},
		{name: "timeout", args: args{command: "sleep 2", timeout: check.Duration(20 * time.Millisecond)},
			want: want{verdict: "inconclusive", cause: "command_error"}},
		{name: "edited pinned adapter", args: args{changed: true},
			want: want{verdict: "inconclusive", cause: "command_error"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			response, command := tc.args.response, tc.args.command
			if response == "" {
				response = criticResponse
			}
			if command == "" {
				command = "printf 'acme raw stderr' >&2; cat response.json"
			}
			r := criticJob(t, f, response, command)
			r.Critic.Command.Timeout = tc.args.timeout
			r = f.j.Pin(r)
			if tc.args.changed {
				require.NoError(t, os.WriteFile(filepath.Join(f.folder, "critic.sh"), []byte("exit 0\n"), 0o700))
			}
			e, err := f.j.Probe(context.Background(), r, f.now, "preview")
			require.NoError(t, err)
			var p ledger.Judgment
			require.NoError(t, e.Decode(&p))
			require.NotNil(t, p.Critic)
			assert.Equal(t, ledger.VerdictPass, p.Verdict)
			assert.Equal(t, tc.want, want{verdict: string(p.Critic.Verdict), cause: p.Critic.Cause, reason: p.Critic.Reason})
			assert.Equal(t, "acme", p.Critic.Source)
			assert.Equal(t, "adapter-v1", p.Critic.SourceRevision)
			assert.Equal(t, "C1", p.Critic.CriteriaRevision)
			assert.Equal(t, f.now, p.Critic.EvaluatedAt)
			b, err := json.Marshal(p.Critic)
			require.NoError(t, err)
			assert.NotContains(t, string(b), "raw stderr")
			assert.NotContains(t, string(b), "critic.sh")
		})
	}
}

func TestCriticReasonBound(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name, input, want string }{
		{name: "two byte boundary", input: strings.Repeat("é", 800), want: strings.Repeat("é", 512)},
		{name: "three byte boundary", input: strings.Repeat("€", 500), want: strings.Repeat("€", 341)},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			r := criticJob(t, f, strings.Replace(criticResponse, "acme note", tc.input, 1), "cat response.json")
			r.Critic.RetainReason = true
			e, err := f.j.Probe(context.Background(), r, f.now, "preview")
			require.NoError(t, err)
			var p ledger.Judgment
			require.NoError(t, e.Decode(&p))
			require.NotNil(t, p.Critic)
			assert.Equal(t, tc.want, p.Critic.Reason)
			assert.Equal(t, ledger.CriticApproved, p.Critic.Verdict)
		})
	}
}

func TestCriticSkipsUnmetChecks(t *testing.T) {
	t.Parallel()
	tcs := []struct{ name, code, want string }{
		{name: "failed check", code: "1", want: "fail"},
		{name: "unavailable check", code: "75", want: "inconclusive"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			r := criticJob(t, f, criticResponse, "cat response.json")
			r.Checks[0].Run = "exit " + tc.code
			e, err := f.j.Probe(context.Background(), r, f.now, "preview")
			require.NoError(t, err)
			var p ledger.Judgment
			require.NoError(t, e.Decode(&p))
			require.NotNil(t, p.Critic)
			assert.Equal(t, tc.want, string(p.Verdict))
			assert.Equal(t, "checks_not_passed", p.Critic.Cause)
			assert.Equal(t, ledger.CriticInconclusive, p.Critic.Verdict)
			_, err = os.Stat(filepath.Join(f.folder, "calls"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestCriticTickRecovery(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name       string
		failAppend bool
		want       string
	}{
		{name: "recorded once", want: "x"},
		{name: "append failure reruns safely", failAppend: true, want: "xx"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			r := f.j.Pin(criticJob(t, f, criticResponse, "cat response.json"))
			f.save(r, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
			at := time.Date(2026, 10, 8, 9, 31, 0, 0, time.UTC)
			if tc.failAppend {
				path := filepath.Join(f.home, "ledger.lock")
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0o700))
				f.now = at
				_, err := f.j.Tick(context.Background())
				require.Error(t, err)
				require.NoError(t, os.Remove(path))
			}
			out := f.tick(at)
			require.Len(t, out.Verdicts, 1)
			assert.Empty(t, f.tick(at.Add(time.Minute)).Verdicts)
			calls, err := os.ReadFile(filepath.Join(f.folder, "calls"))
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(calls))
			probe, err := f.j.Probe(context.Background(), r, at.Add(2*time.Minute), "explicit retry")
			require.NoError(t, err)
			assert.NotEqual(t, out.Verdicts[0].ID, probe.ID)
			assert.Equal(t, ledger.KindProbe, probe.Kind)
			calls, err = os.ReadFile(filepath.Join(f.folder, "calls"))
			require.NoError(t, err)
			assert.Equal(t, tc.want+"x", string(calls))
		})
	}
}
