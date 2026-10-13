package judge

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

const (
	// The hook runs inside the 1.5 s SessionEnd budget, so it never reads past the head
	transcriptHead = 256 << 10
	maxPromptChars = 200
)

var errNoPrompt = errors.New("no user prompt in the transcript head")

// One transcript line, only the fields a session record needs
type transcriptLine struct {
	Type      string    `json:"type"`
	SessionID string    `json:"sessionId"`
	Cwd       string    `json:"cwd"`
	Timestamp time.Time `json:"timestamp"`
	IsMeta    bool      `json:"isMeta"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// ReadSession reads who started a session, where and when, from the head of its transcript
// The first real user prompt is the one a scheduled run sends, so it carries the job marker
func ReadSession(path string) (ledger.Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return ledger.Session{}, err
	}
	defer func() { _ = f.Close() }()
	s := ledger.Session{TranscriptPath: path}
	r := bufio.NewReader(io.LimitReader(f, transcriptHead))
	for {
		raw, err := r.ReadBytes('\n')
		var line transcriptLine
		if json.Unmarshal(raw, &line) == nil {
			fill(&s, line)
			if s.FirstPrompt != "" {
				return s, nil
			}
		}
		if err != nil {
			return s, errNoPrompt
		}
	}
}

func fill(s *ledger.Session, line transcriptLine) {
	if s.SessionID == "" {
		s.SessionID = line.SessionID
	}
	if s.Cwd == "" {
		s.Cwd = line.Cwd
	}
	// Lines aren't written in time order, so the start is the earliest stamp seen
	if !line.Timestamp.IsZero() && (s.StartedAt.IsZero() || line.Timestamp.Before(s.StartedAt)) {
		s.StartedAt = line.Timestamp.UTC()
	}
	if line.Type != "user" || line.IsMeta || line.Message.Role != "user" {
		return
	}
	text := strings.TrimSpace(promptText(line.Message.Content))
	// Slash commands and their output arrive as user lines wrapped in tags like <command-name>
	if text == "" || strings.HasPrefix(text, "<") {
		return
	}
	s.FirstPrompt = clip(firstLine(text), maxPromptChars)
}

// Content is plain text or a list of blocks, and tool results come back as user lines too
func promptText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return b.Text
		}
	}
	return ""
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
