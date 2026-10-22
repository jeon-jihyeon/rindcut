package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jeon-jihyeon/rindcut/internal/ledger"
)

// Settings is exporters.json, written by the plugin and read by every tick
// The tick runs from launchd or systemd with no shell environment, so nothing here comes from env
type Settings struct {
	OTLP     *OTLPSettings     `json:"otlp,omitempty"`
	Langfuse *LangfuseSettings `json:"langfuse-score,omitempty"`
	// Nil means automatic: on when a nodloop binary is found
	Nodloop *NodloopSettings `json:"nodloop,omitempty"`
}

func LoadSettings(path string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// SaveSettings writes owner-only, since the file holds API keys
func SaveSettings(path string, s Settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	return os.Rename(tmp, path)
}

// Masked hides keys and auth headers, for every place settings are shown
func (s Settings) Masked() Settings {
	out := s
	if s.OTLP != nil {
		o := *s.OTLP
		o.Headers = map[string]string{}
		for k, v := range s.OTLP.Headers {
			o.Headers[k] = mask(v)
		}
		out.OTLP = &o
	}
	if s.Langfuse != nil {
		l := *s.Langfuse
		l.PublicKey, l.SecretKey = mask(l.PublicKey), mask(l.SecretKey)
		out.Langfuse = &l
	}
	return out
}

func mask(v string) string {
	if len(v) <= 8 {
		return strings.Repeat("*", len(v))
	}
	return v[:4] + strings.Repeat("*", 8) + v[len(v)-2:]
}

// Enabled names the exporters a run would use, in the order Build makes them, without making any
// searchPath is the PATH to look for nodloop in
func (s Settings) Enabled(userHome, searchPath string) []string {
	var out []string
	if s.OTLP != nil && s.OTLP.Enabled {
		out = append(out, NameOTLP)
	}
	if s.Langfuse != nil && s.Langfuse.Enabled {
		out = append(out, NameLangfuse)
	}
	if (s.Nodloop == nil || s.Nodloop.Enabled) && FindNodloop(userHome, searchPath) != "" {
		out = append(out, NameNodloop)
	}
	return out
}

// Build makes the enabled exporters for one run
// names maps job ids to names for trace titles, entries let nodloop find the verdict a correction fixes
func (s Settings) Build(ctx context.Context, entries []ledger.Entry, names map[string]string, version, userHome, searchPath, cursorDir string) ([]Exporter, error) {
	var out []Exporter
	for _, name := range s.Enabled(userHome, searchPath) {
		switch name {
		case NameOTLP:
			o, err := NewOTLP(ctx, *s.OTLP, names, version)
			if err != nil {
				return nil, err
			}
			out = append(out, o)
		case NameLangfuse:
			out = append(out, NewLangfuseScore(*s.Langfuse))
		case NameNodloop:
			out = append(out, NewNodloop(FindNodloop(userHome, searchPath), filepath.Join(cursorDir, "nodloop-bindings.json"), entries))
		}
	}
	return out, nil
}

// Replace sets one exporter's settings as a whole, so a header or key left out is gone rather than kept from before
func (s *Settings) Replace(name string, raw json.RawMessage) error {
	var err error
	switch name {
	case NameOTLP:
		s.OTLP = &OTLPSettings{}
		err = json.Unmarshal(raw, s.OTLP)
	case NameLangfuse:
		s.Langfuse = &LangfuseSettings{}
		err = json.Unmarshal(raw, s.Langfuse)
	case NameNodloop:
		s.Nodloop = &NodloopSettings{}
		err = json.Unmarshal(raw, s.Nodloop)
	default:
		return fmt.Errorf("unknown exporter %q, use %s, %s or %s", name, NameOTLP, NameLangfuse, NameNodloop)
	}
	if err != nil {
		return fmt.Errorf("%s settings: %w", name, err)
	}
	return nil
}
