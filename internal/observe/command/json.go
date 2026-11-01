package command

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var integer = regexp.MustCompile(`^(0|-[1-9][0-9]*|[1-9][0-9]*)$`)

// Decode rejects ambiguity before the standard decoder can normalize it
func Decode(data []byte, target any) error {
	if len(data) > MaxOutput {
		return &Error{Code: "output_limit"}
	}
	if !utf8.Valid(data) || !validEscapes(data) {
		return &Error{Code: "invalid_response"}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := jsonValue(d, 0)
	if err != nil {
		return &Error{Code: "invalid_response"}
	}
	if _, err = d.Token(); err != io.EOF {
		return &Error{Code: "invalid_response"}
	}
	t := reflect.TypeOf(target)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return &Error{Code: "invalid_response"}
	}
	if !shape(v, t.Elem(), false) {
		return &Error{Code: "invalid_response"}
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err = d.Decode(target); err != nil {
		return &Error{Code: "invalid_response"}
	}
	return nil
}

func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, &Error{Code: "invalid_response"}
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	if n, ok := t.(json.Number); ok {
		i, e := strconv.ParseInt(n.String(), 10, 64)
		if !integer.MatchString(n.String()) || e != nil || i < -9007199254740991 || i > 9007199254740991 {
			return nil, &Error{Code: "invalid_response"}
		}
		return n, nil
	}
	if delim, ok := t.(json.Delim); ok {
		return jsonContainer(d, delim, depth)
	}
	return t, nil
}

func jsonContainer(d *json.Decoder, delim json.Delim, depth int) (any, error) {
	if delim == '{' {
		obj := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			s, ok := key.(string)
			if !ok {
				return nil, &Error{Code: "invalid_response"}
			}
			if _, exists := obj[s]; exists {
				return nil, &Error{Code: "invalid_response"}
			}
			obj[s], err = jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
		}
		_, err := d.Token()
		return obj, err
	}
	if delim != '[' {
		return nil, &Error{Code: "invalid_response"}
	}
	arr := []any{}
	for d.More() {
		v, err := jsonValue(d, depth+1)
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
	}
	_, err := d.Token()
	return arr, err
}

func shape(v any, t reflect.Type, nullable bool) bool {
	if v == nil {
		return nullable
	}
	if t.Kind() == reflect.Pointer {
		return shape(v, t.Elem(), false)
	}
	if t == reflect.TypeFor[time.Time]() {
		text, ok := v.(string)
		if !ok {
			return false
		}
		_, ok = timeValue(text)
		return ok
	}
	switch t.Kind() {
	case reflect.Struct:
		return shapeObject(v, t)
	case reflect.Slice:
		return shapeArray(v, t)
	case reflect.Map:
		return shapeMap(v, t)
	case reflect.Interface:
		switch v.(type) {
		case string, bool, json.Number:
			return true
		default:
			return false
		}
	case reflect.String:
		_, ok := v.(string)
		return ok
	case reflect.Bool:
		_, ok := v.(bool)
		return ok
	case reflect.Int, reflect.Int64:
		_, ok := v.(json.Number)
		return ok
	default:
		return false
	}
}

func shapeMap(v any, t reflect.Type) bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, value := range obj {
		if !shape(value, t.Elem(), false) {
			return false
		}
	}
	return true
}

func shapeObject(v any, t reflect.Type) bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	fields := jsonFields(t)
	for key, value := range obj {
		f, exists := fields[key]
		if !exists || !shape(value, f.Type, f.Tag.Get("nullable") == "true") {
			return false
		}
		if f.Tag.Get("positive") == "true" {
			n, ok := value.(json.Number)
			if !ok {
				return false
			}
			i, err := n.Int64()
			if err != nil || i < 1 {
				return false
			}
		}
	}
	for key, f := range fields {
		if _, exists := obj[key]; !exists && !strings.Contains(f.Tag.Get("json"), ",omitempty") {
			return false
		}
	}
	return true
}

func shapeArray(v any, t reflect.Type) bool {
	arr, ok := v.([]any)
	if !ok {
		return false
	}
	for _, value := range arr {
		if !shape(value, t.Elem(), false) {
			return false
		}
	}
	return true
}

func jsonFields(t reflect.Type) map[string]reflect.StructField {
	fields := map[string]reflect.StructField{}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			for k, value := range jsonFields(f.Type) {
				fields[k] = value
			}
			continue
		}
		key := strings.Split(f.Tag.Get("json"), ",")[0]
		if key != "" && key != "-" {
			fields[key] = f
		}
	}
	return fields
}

func validEscapes(data []byte) bool {
	quoted := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		var ok bool
		i, ok = unicodeEscape(data, i)
		if !ok {
			return false
		}
	}
	return !quoted
}

func unicodeEscape(data []byte, i int) (int, bool) {
	if i+4 >= len(data) {
		return i, false
	}
	n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
	if err != nil || n >= 0xdc00 && n <= 0xdfff {
		return i, false
	}
	i += 4
	if n < 0xd800 || n > 0xdbff {
		return i, true
	}
	if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
		return i, false
	}
	low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
	return i + 6, err == nil && low >= 0xdc00 && low <= 0xdfff
}
