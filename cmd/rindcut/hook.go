package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// The SessionEnd hook input, only the fields a session record needs
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Reason         string `json:"reason"`
}

// runHook records one session within the shared 1.5 s SessionEnd budget
// A session the hook misses is recovered later by the tick's transcript backfill
func runHook(args []string, e env) error {
	if len(args) != 1 || args[0] != "session-end" {
		return fmt.Errorf("want `hook session-end`, got %v", args)
	}
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(e.stdin, 1<<20)).Decode(&in); err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	a, err := newApp(e)
	if err != nil {
		return err
	}
	_, err = a.RecordSession(context.Background(), ledger.Session{
		SessionID: in.SessionID, TranscriptPath: in.TranscriptPath, Cwd: in.Cwd, EndReason: in.Reason,
	})
	return err
}
