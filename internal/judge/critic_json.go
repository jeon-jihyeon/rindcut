package judge

import (
	"encoding/json"
	"io"
	"strings"
)

func decodeCritic(text string, out *criticOutput) bool {
	if !uniqueJSON(json.NewDecoder(strings.NewReader(text))) {
		return false
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return false
	}
	return d.Decode(new(any)) == io.EOF
}

// Duplicate keys could replace an earlier incomplete or rejected judgment
func uniqueJSON(d *json.Decoder) bool {
	token, err := d.Token()
	if err != nil {
		return false
	}
	opening, compound := token.(json.Delim)
	if !compound {
		return true
	}
	seen := map[string]bool{}
	for d.More() {
		if opening == '{' {
			key, err := d.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok || name != strings.ToLower(name) || seen[name] {
				return false
			}
			seen[name] = true
		}
		if !uniqueJSON(d) {
			return false
		}
	}
	_, err = d.Token()
	return err == nil
}
