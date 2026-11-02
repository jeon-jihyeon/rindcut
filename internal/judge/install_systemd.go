package judge

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func systemdOwner(body []byte) string {
	var values []string
	section := ""
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = line
		}
		if section == "[Service]" && strings.HasPrefix(line, "Environment=") {
			words, ok := scheduleWords(strings.TrimPrefix(line, "Environment="))
			if !ok {
				return ""
			}
			values = append(values, words...)
		}
	}
	env, ok := scheduleEnvironment(values)
	if !ok {
		return ""
	}
	home := env["RINDCUT_HOME"]
	if strings.Contains(strings.ReplaceAll(home, "%%", ""), "%") {
		return ""
	}
	return strings.ReplaceAll(home, "%%", "%")
}

func (in Installer) inspectSystemd(ctx context.Context) (ScheduleStatus, bool) {
	units, status := in.querySystemd(ctx)
	if status.State != "" {
		return status, false
	}
	timer, service := units[0], units[1]
	if timer["LoadState"] == "not-found" && service["LoadState"] == "not-found" {
		return ScheduleStatus{State: "absent", Reason: "not_registered"}, false
	}
	if status = in.systemdServiceOwner(service); status.State != "ready" {
		return status, true
	}
	status = in.systemdTimer(timer)
	if status.State == "foreign" || status.State == "unavailable" {
		return status, true
	}
	if !in.systemdServiceMatches(service) {
		return ScheduleStatus{State: "inactive", Reason: "config_drift"}, true
	}
	return status, true
}

func (in Installer) querySystemd(ctx context.Context) ([]map[string]string, ScheduleStatus) {
	properties := []string{
		"LoadState,ActiveState,UnitFileState,FragmentPath,DropInPaths,Unit,TimersCalendar,TimersMonotonic,Persistent",
		"LoadState,FragmentPath,DropInPaths,ExecStart,Environment",
	}
	units := make([]map[string]string, 2)
	for i, suffix := range []string{".timer", ".service"} {
		out, err := in.run(ctx, "systemctl", "--user", "show", in.name()+suffix,
			"--property="+properties[i], "--all")
		if err != nil {
			return nil, ScheduleStatus{State: "unavailable", Reason: "query_failed"}
		}
		var ok bool
		units[i], ok = systemdProperties(string(out), strings.Split(properties[i], ","))
		if !ok {
			return nil, ScheduleStatus{State: "unavailable", Reason: "query_format"}
		}
	}
	return units, ScheduleStatus{}
}

func (in Installer) systemdServiceOwner(service map[string]string) ScheduleStatus {
	if service["LoadState"] != "loaded" {
		return ScheduleStatus{State: "foreign", Reason: "owner_unknown"}
	}
	words, ok := scheduleWords(service["Environment"])
	env, valid := scheduleEnvironment(words)
	if !ok || !valid {
		return ScheduleStatus{State: "unavailable", Reason: "query_format"}
	}
	if env["RINDCUT_HOME"] == "" {
		return ScheduleStatus{State: "foreign", Reason: "owner_unknown"}
	}
	if env["RINDCUT_HOME"] != in.Home {
		return ScheduleStatus{State: "foreign", Reason: "home_mismatch"}
	}
	if !sameSchedulePath(service["FragmentPath"], filepath.Join(in.unitDir(), in.name()+".service")) ||
		service["DropInPaths"] != "" {
		return ScheduleStatus{State: "foreign", Reason: "target_mismatch"}
	}
	return ScheduleStatus{State: "ready", Reason: "registered"}
}

func (in Installer) systemdServiceMatches(service map[string]string) bool {
	words, ok := scheduleWords(service["Environment"])
	env, valid := scheduleEnvironment(words)
	return ok && valid && systemdExec(service["ExecStart"], in.Binary) && env["HOME"] == in.UserHome &&
		env["CLAUDE_CONFIG_DIR"] == in.ClaudeConfigDir
}

func (in Installer) systemdTimer(timer map[string]string) ScheduleStatus {
	if timer["LoadState"] == "not-found" {
		return ScheduleStatus{State: "inactive", Reason: "not_loaded"}
	}
	if timer["LoadState"] != "loaded" {
		return ScheduleStatus{State: "unavailable", Reason: "query_format"}
	}
	if !sameSchedulePath(timer["FragmentPath"], filepath.Join(in.unitDir(), in.name()+".timer")) ||
		timer["DropInPaths"] != "" || timer["Unit"] != in.name()+".service" {
		return ScheduleStatus{State: "foreign", Reason: "target_mismatch"}
	}
	if !systemdCalendar(timer) {
		return ScheduleStatus{State: "inactive", Reason: "config_drift"}
	}
	if timer["UnitFileState"] != "enabled" {
		return ScheduleStatus{State: "inactive", Reason: "not_enabled"}
	}
	if timer["ActiveState"] != "active" {
		return ScheduleStatus{State: "inactive", Reason: "timer_inactive"}
	}
	return ScheduleStatus{State: "ready", Reason: "registered"}
}

func systemdCalendar(timer map[string]string) bool {
	value := timer["TimersCalendar"]
	return strings.HasPrefix(value, "{ OnCalendar=*-*-* *:00/5:00 ; next_elapse=") &&
		strings.HasSuffix(value, " }") && !strings.Contains(value, "} {") &&
		timer["TimersMonotonic"] == "" && timer["Persistent"] == "yes"
}

func systemdProperties(out string, keys []string) (map[string]string, bool) {
	properties := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, false
		}
		if _, exists := properties[key]; exists {
			return nil, false
		}
		properties[key] = value
	}
	for _, key := range keys {
		if _, exists := properties[key]; !exists {
			if !emptySystemdProperty(key, properties["LoadState"]) {
				return nil, false
			}
			properties[key] = ""
		}
	}
	return properties, len(properties) == len(keys)
}

func emptySystemdProperty(key, state string) bool {
	switch key {
	case "TimersCalendar", "TimersMonotonic":
		return true
	case "ExecStart":
		return state == "not-found"
	default:
		return false
	}
}

func systemdExec(value, binary string) bool {
	prefix := "{ path=" + binary + " ; argv[]=" + binary + " tick ; "
	return strings.HasPrefix(value, prefix) && strings.HasSuffix(value, " }") &&
		!strings.Contains(value[len(prefix):], "{ path=")
}

func scheduleEnvironment(words []string) (map[string]string, bool) {
	env := make(map[string]string)
	for _, word := range words {
		key, value, ok := strings.Cut(word, "=")
		if !ok || key == "" {
			return nil, false
		}
		if _, exists := env[key]; exists {
			return nil, false
		}
		env[key] = value
	}
	return env, true
}

func scheduleWords(value string) ([]string, bool) {
	var words []string
	for value = strings.TrimSpace(value); value != ""; value = strings.TrimSpace(value) {
		var word strings.Builder
		var quote byte
		for len(value) > 0 {
			c := value[0]
			if quote == 0 && (c == ' ' || c == '\t') {
				break
			}
			if c == '"' || c == '\'' {
				if quote == 0 {
					quote = c
					value = value[1:]
					continue
				}
				if quote == c {
					quote = 0
					value = value[1:]
					continue
				}
			}
			if c == '\\' {
				if strings.HasPrefix(value, `\s`) {
					word.WriteByte(' ')
					value = value[2:]
					continue
				}
				r, _, tail, err := strconv.UnquoteChar(value, '"')
				if err != nil {
					return nil, false
				}
				word.WriteRune(r)
				value = tail
				continue
			}
			word.WriteByte(c)
			value = value[1:]
		}
		if quote != 0 {
			return nil, false
		}
		words = append(words, word.String())
	}
	return words, true
}

func (in Installer) Units() (service, timer []byte) {
	q := func(s string) string { return strconv.Quote(strings.ReplaceAll(s, "%", "%%")) }
	config := ""
	if in.ClaudeConfigDir != "" {
		config = "\nEnvironment=" + q("CLAUDE_CONFIG_DIR="+in.ClaudeConfigDir)
	}
	service = fmt.Appendf(nil, `[Unit]
Description=rindcut tick: judge scheduled AI jobs

[Service]
Type=oneshot
Environment=%s
Environment=%s%s
ExecStart=%s tick
`, q("HOME="+in.UserHome), q("RINDCUT_HOME="+in.Home), config, q(in.Binary))
	timer = fmt.Appendf(nil, `[Unit]
Description=rindcut tick every 5 minutes

[Timer]
OnCalendar=*:0/5
Persistent=true

[Install]
WantedBy=timers.target
`)
	return service, timer
}
