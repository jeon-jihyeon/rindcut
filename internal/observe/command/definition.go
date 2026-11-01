package command

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

func ParseDefinition(data []byte) (Definition, error) {
	var d Definition
	if err := Decode(data, &d); err != nil {
		return Definition{}, err
	}
	if explicitEmptyEnums(data) {
		return Definition{}, &Error{Code: "invalid_definition"}
	}
	if err := d.Validate(); err != nil {
		return Definition{}, err
	}
	if d.Reader.TimeoutSeconds == 0 {
		d.Reader.TimeoutSeconds = 30
	}
	if d.Reader.Limit == 0 {
		d.Reader.Limit = 100
	}
	for i := range d.Observers {
		o := &d.Observers[i]
		if o.TimeoutSeconds == 0 {
			o.TimeoutSeconds = 30
		}
		if o.IntervalSeconds == 0 {
			o.IntervalSeconds = 60
		}
		if o.Mode == "" {
			o.Mode = "until_terminal"
		}
		if o.ActorKind == "" {
			o.ActorKind = "code"
		}
	}
	return d, nil
}

func explicitEmptyEnums(data []byte) bool {
	var values struct {
		Observers []struct {
			Mode      *string `json:"mode"`
			ActorKind *string `json:"actor_kind"`
		} `json:"observers"`
	}
	if err := json.Unmarshal(data, &values); err != nil {
		return true
	}
	for _, observer := range values.Observers {
		if observer.Mode != nil && *observer.Mode == "" || observer.ActorKind != nil && *observer.ActorKind == "" {
			return true
		}
	}
	return false
}

func (d Definition) Validate() error {
	if !identifiers([]string{d.ID, d.SourceInstance, d.Stream, d.AdapterRevision, d.ProjectionProfile}) ||
		!validCapability(d.Capability) || !validReader(d.Reader) || d.Observers == nil || len(d.Observers) > 100 ||
		len(d.Description) > 2048 || !utf8.ValidString(d.Description) {
		return &Error{Code: "invalid_definition"}
	}
	seen := map[string]bool{}
	for _, o := range d.Observers {
		if seen[o.ID] || !validObserver(o) {
			return &Error{Code: "invalid_definition"}
		}
		seen[o.ID] = true
	}
	data, err := json.Marshal(d)
	if err != nil || len(data) > MaxOutput {
		return &Error{Code: "invalid_definition"}
	}
	return nil
}

func validReader(r Reader) bool {
	if !validProgram(r.Program, r.Args, r.FingerprintFiles, r.TimeoutSeconds) || r.AllowedAttributes == nil ||
		len(r.AllowedAttributes) > 32 || r.Limit < 0 || r.Limit > 100 {
		return false
	}
	seen := map[string]bool{}
	for _, key := range r.AllowedAttributes {
		if !ascii.MatchString(key) || seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

func validObserver(o Observer) bool {
	return identifiers([]string{o.ID, o.CriterionID, o.CriterionRevision}) && len(o.Types) > 0 &&
		identifiers(o.Types) && attributes(o.Parameters) && validProgram(o.Program, o.Args, o.FingerprintFiles, o.TimeoutSeconds) &&
		o.DeadlineSeconds >= 1 && o.DeadlineSeconds <= 31536000 && o.IntervalSeconds >= 0 &&
		o.IntervalSeconds <= 31536000 && (o.Mode == "" || o.Mode == "until_terminal" || o.Mode == "watch") &&
		(o.ActorKind == "" || o.ActorKind == "code" || o.ActorKind == "model")
}

func validProgram(program string, args, files []string, timeout int) bool {
	if program == "" || !utf8.ValidString(program) || strings.ContainsRune(program, 0) || args == nil ||
		len(args) > 128 || len(files) > 16 || timeout < 0 || timeout > 600 {
		return false
	}
	return validArgs(args) && validPaths(files)
}

func validArgs(args []string) bool {
	for _, arg := range args {
		if len(arg) > 2048 || !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return false
		}
	}
	return true
}

func validPaths(files []string) bool {
	for _, path := range files {
		if path == "" || !utf8.ValidString(path) || strings.ContainsRune(path, 0) {
			return false
		}
	}
	return true
}
