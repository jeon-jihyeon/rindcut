package export_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/export"
	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

type score struct {
	ID, TraceID, Name, Value, DataType, Comment string
}

// langfuse answers each request with status and records what it got
type langfuse struct {
	mu     sync.Mutex
	status int
	got    []score
	paths  []string
}

func (l *langfuse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	user, pass, _ := r.BasicAuth()
	if user != "pk" || pass != "sk" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var s score
	_ = json.NewDecoder(r.Body).Decode(&s)
	l.got, l.paths = append(l.got, s), append(l.paths, r.Method+" "+r.URL.Path)
	w.WriteHeader(l.status)
}

func TestLangfuseScores(t *testing.T) {
	t.Parallel()
	lf := &langfuse{status: http.StatusOK}
	srv := httptest.NewServer(lf)
	t.Cleanup(srv.Close)
	l := export.NewLangfuseScore(export.LangfuseSettings{Host: srv.URL + "/", PublicKey: "pk", SecretKey: "sk"})
	es := []ledger.Entry{
		verdict(t, "v1", ledger.VerdictFail),
		entry(t, "c1", ledger.KindCorrection, ledger.Correction{VerdictID: "v1", Verdict: ledger.VerdictPass, Reason: "renamed"}),
	}
	require.NoError(t, l.Export(context.Background(), es))
	assert.Equal(t, []score{
		{ID: "v1", TraceID: export.TraceID("v1"), Name: "rindcut.verdict", Value: "fail", DataType: "CATEGORICAL"},
		{ID: "c1", TraceID: export.TraceID("v1"), Name: "rindcut.correction", Value: "pass", DataType: "CATEGORICAL", Comment: "renamed"},
	}, lf.got)
	require.NoError(t, l.Test(context.Background()))
	assert.Equal(t, "GET /api/public/projects", lf.paths[len(lf.paths)-1])
}

func TestLangfuseErrors(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name      string
		status    int
		key       string
		permanent bool
	}{
		{name: "bad request is permanent", status: http.StatusBadRequest, key: "sk", permanent: true},
		{name: "wrong key is permanent", status: http.StatusOK, key: "wrong", permanent: true},
		{name: "rate limit is retried", status: http.StatusTooManyRequests, key: "sk"},
		{name: "server error is retried", status: http.StatusBadGateway, key: "sk"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(&langfuse{status: tc.status})
			t.Cleanup(srv.Close)
			l := export.NewLangfuseScore(export.LangfuseSettings{Host: srv.URL, PublicKey: "pk", SecretKey: tc.key})
			err := l.Export(context.Background(), []ledger.Entry{verdict(t, "v1", ledger.VerdictPass)})
			require.Error(t, err)
			var permanent export.PermanentError
			assert.Equal(t, tc.permanent, errors.As(err, &permanent))
		})
	}
}
