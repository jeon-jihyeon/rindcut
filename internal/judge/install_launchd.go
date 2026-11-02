package judge

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

type plistNode struct {
	XMLName  xml.Name
	Text     string      `xml:",chardata"`
	Children []plistNode `xml:",any"`
}

func launchdOwner(body []byte) (string, string) {
	var root plistNode
	if xml.Unmarshal(body, &root) != nil || root.XMLName.Local != "plist" || len(root.Children) != 1 {
		return "", ""
	}
	dict := plistValues(root.Children[0])
	label := dict["Label"]
	if label.XMLName.Local != "string" {
		return "", ""
	}
	env := plistValues(dict["EnvironmentVariables"])
	if home := env["RINDCUT_HOME"]; home.XMLName.Local == "string" {
		return home.Text, label.Text
	}
	return "", label.Text
}

func plistValues(node plistNode) map[string]plistNode {
	if node.XMLName.Local != "dict" || len(node.Children)%2 != 0 {
		return nil
	}
	values := make(map[string]plistNode)
	for i := 0; i < len(node.Children); i += 2 {
		key := node.Children[i]
		if key.XMLName.Local != "key" || key.Text == "" {
			return nil
		}
		if _, exists := values[key.Text]; exists {
			return nil
		}
		values[key.Text] = node.Children[i+1]
	}
	return values
}

func (in Installer) inspectLaunchd(ctx context.Context) (ScheduleStatus, bool, bool) {
	disabled, err := in.run(ctx, "launchctl", "print-disabled", in.domain())
	if err != nil {
		return ScheduleStatus{State: "unavailable", Reason: "query_failed"}, false, false
	}
	off, ok := launchdDisabled(string(disabled), in.name())
	if !ok {
		return ScheduleStatus{State: "unavailable", Reason: "query_format"}, false, false
	}
	out, err := in.run(ctx, "launchctl", "print", in.target())
	if err != nil {
		var code interface{ ExitCode() int }
		if errors.As(err, &code) && code.ExitCode() == 113 {
			return ScheduleStatus{State: "absent", Reason: "not_registered"}, false, off
		}
		return ScheduleStatus{State: "unavailable", Reason: "query_failed"}, false, off
	}
	return in.loadedLaunchd(string(out), off), true, off
}

func (in Installer) loadedLaunchd(out string, off bool) ScheduleStatus {
	fields, args, env, ok := launchdProperties(out, in.target())
	if !ok {
		return ScheduleStatus{State: "unavailable", Reason: "query_format"}
	}
	if env["RINDCUT_HOME"] == "" {
		return ScheduleStatus{State: "foreign", Reason: "owner_unknown"}
	}
	if env["RINDCUT_HOME"] != in.Home {
		return ScheduleStatus{State: "foreign", Reason: "home_mismatch"}
	}
	if !sameSchedulePath(fields["path"], in.plistPath()) {
		return ScheduleStatus{State: "foreign", Reason: "target_mismatch"}
	}
	if !in.launchdConfiguration(fields, args, env) {
		return ScheduleStatus{State: "inactive", Reason: "config_drift"}
	}
	switch fields["state"] {
	case "running", "not running", "waiting", "spawn scheduled", "xpcproxy":
	default:
		return ScheduleStatus{State: "unavailable", Reason: "query_format"}
	}
	if off {
		return ScheduleStatus{State: "inactive", Reason: "disabled"}
	}
	return ScheduleStatus{State: "ready", Reason: "registered"}
}

func (in Installer) launchdConfiguration(fields map[string]string, args []string, env map[string]string) bool {
	return fields["program"] == in.Binary && len(args) == 2 && args[0] == in.Binary && args[1] == "tick" &&
		env["HOME"] == in.UserHome && env["CLAUDE_CONFIG_DIR"] == in.ClaudeConfigDir &&
		fields["run interval"] == "300 seconds"
}

func launchdDisabled(out, name string) (bool, bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "disabled services = {" ||
		strings.TrimSpace(lines[len(lines)-1]) != "}" {
		return false, false
	}
	seen, disabled := false, false
	for _, line := range lines[1 : len(lines)-1] {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " => ")
		if !ok || !strings.HasPrefix(key, `"`) || !strings.HasSuffix(key, `"`) ||
			(value != "true" && value != "false" && value != "enabled" && value != "disabled") {
			return false, false
		}
		if key == strconv.Quote(name) {
			if seen {
				return false, false
			}
			seen, disabled = true, value == "true" || value == "disabled"
		}
	}
	return disabled, true
}

// Conservative parsing for launchctl output without a stable machine format
func launchdProperties(out, target string) (map[string]string, []string, map[string]string, bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != target+" = {" {
		return nil, nil, nil, false
	}
	p := launchdPrint{fields: make(map[string]string), env: make(map[string]string),
		seen: make(map[string]bool), depth: 1}
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !p.line(line) {
			return nil, nil, nil, false
		}
	}
	return p.fields, p.args, p.env, p.depth == 0 && len(p.fields) == 4 && p.seen["arguments"] && p.seen["environment"]
}

type launchdPrint struct {
	fields, env map[string]string
	seen        map[string]bool
	args        []string
	depth       int
	section     string
}

func (p *launchdPrint) line(line string) bool {
	switch {
	case line == "}":
		p.depth--
		if p.depth == 1 {
			p.section = ""
		}
		return p.depth >= 0
	case p.depth == 0:
		return false
	case p.depth == 1:
		return p.property(line)
	case p.depth == 2 && p.section == "arguments":
		p.args = append(p.args, line)
	case p.depth == 2 && p.section == "environment":
		key, value, ok := strings.Cut(line, " => ")
		if !ok {
			return false
		}
		if _, exists := p.env[key]; exists {
			return false
		}
		p.env[key] = value
	case strings.HasSuffix(line, " = {"), strings.HasSuffix(line, " => {"):
		p.depth++
	}
	return true
}

func (p *launchdPrint) property(line string) bool {
	key, value, ok := strings.Cut(line, " = ")
	if !ok {
		return true
	}
	if value == "{" {
		p.section = key
		p.depth++
		if key == "arguments" || key == "environment" {
			if p.seen[key] {
				return false
			}
			p.seen[key] = true
		}
		return true
	}
	switch key {
	case "path", "program", "state", "run interval":
		if _, exists := p.fields[key]; exists {
			return false
		}
		p.fields[key] = value
	}
	return true
}

// Explicit environment for background sessions without the calling shell
func (in Installer) Plist() []byte {
	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	config := ""
	if in.ClaudeConfigDir != "" {
		config = "\n    <key>CLAUDE_CONFIG_DIR</key><string>" + esc(in.ClaudeConfigDir) + "</string>"
	}
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>tick</string></array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>%s</string>
    <key>RINDCUT_HOME</key><string>%s</string>%s
  </dict>
  <key>StartInterval</key><integer>%d</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, esc(in.name()), esc(in.Binary), esc(in.UserHome), esc(in.Home), config,
		tickSeconds, esc(filepath.Join(in.Home, "tick.err.log")))
	return b.Bytes()
}
