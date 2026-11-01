package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return checkDir(path)
}

func checkDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return ErrCorrupt
	}
	return nil
}

func openFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrCorrupt
	}
	return f, nil
}

func acquire(ctx context.Context, path string, shared, create bool) (func(), error) {
	flags := os.O_RDWR
	if shared {
		flags = os.O_RDONLY
	}
	if create {
		flags |= os.O_CREATE
	}
	f, err := openFile(path, flags)
	if err != nil {
		return nil, err
	}
	op := syscall.LOCK_EX | syscall.LOCK_NB
	if shared {
		op = syscall.LOCK_SH | syscall.LOCK_NB
	}
	deadline := time.Now().Add(time.Second)
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), op)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (s *Store) appendLock(ctx context.Context) (func(), error) {
	return acquire(ctx, filepath.Join(s.home, "observe-append.lock"), false, true)
}

func (s *Store) readLock(ctx context.Context) (func(), bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	info, err := os.Lstat(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, ErrCorrupt
	}
	if info.Mode().Perm() != 0o700 {
		if s.hasArtifacts() {
			return nil, false, ErrCorrupt
		}
		return nil, false, nil
	}
	unlock, err := acquire(ctx, filepath.Join(s.home, "observe-append.lock"), true, false)
	if errors.Is(err, os.ErrNotExist) {
		for _, path := range []string{s.path(""), filepath.Join(s.home, "observe-recovery.json")} {
			if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
				return nil, false, ErrCorrupt
			}
		}
		return nil, false, nil
	}
	return unlock, err == nil, err
}

func (s *Store) hasArtifacts() bool {
	for _, path := range []string{s.path(""), filepath.Join(s.home, "observe-append.lock"),
		filepath.Join(s.home, "observe-control.lock"), s.recoveryPath()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func readJSON(path string, dst any) error {
	f, err := openFile(path, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.Size() >= maxLine {
		return ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLine))
	if err != nil {
		return err
	}
	return decode(data, dst)
}

func decode(data []byte, dst any) error {
	if !validUnicode(data) {
		return ErrCorrupt
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := validateValue(d); err != nil {
		return ErrCorrupt
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return ErrCorrupt
	}
	if err := validateShape(data, reflect.TypeOf(dst)); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return ErrCorrupt
	}
	return nil
}

func validateShape(data []byte, target reflect.Type) error {
	if target == nil || target.Kind() != reflect.Pointer {
		return ErrCorrupt
	}
	return validateField(data, target.Elem())
}

func validateField(data []byte, target reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return ErrCorrupt
	}
	if target.Kind() != reflect.Struct || target == reflect.TypeFor[time.Time]() {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return ErrCorrupt
	}
	known := make(map[string]bool, target.NumField())
	for i := range target.NumField() {
		field := target.Field(i)
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		known[name] = true
		value, exists := object[name]
		if !exists && strings.Contains(options, "omitempty") {
			continue
		}
		if !exists {
			return ErrCorrupt
		}
		if err := validateField(value, field.Type); err != nil {
			return err
		}
	}
	for name := range object {
		if !known[name] {
			return ErrCorrupt
		}
	}
	return nil
}

func validUnicode(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	inside := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		var valid bool
		i, valid = unicodeEscape(data, i)
		if !valid {
			return false
		}
	}
	return true
}

func unicodeEscape(data []byte, i int) (int, bool) {
	if i+4 >= len(data) {
		return i, false
	}
	value, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
	if err != nil || value >= 0xdc00 && value <= 0xdfff {
		return i, false
	}
	i += 4
	if value < 0xd800 || value > 0xdbff {
		return i, true
	}
	if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
		return i, false
	}
	low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
	return i + 6, err == nil && low >= 0xdc00 && low <= 0xdfff
}

func validateValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return ErrCorrupt
	}
	seen := make(map[string]bool)
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrCorrupt
			}
			seen[name] = true
		}
		if err := validateValue(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

func writeAtomic(path string, data []byte) error {
	temp := path + ".tmp"
	f, err := openFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if errors.Is(err, os.ErrExist) {
		if _, e := openExistingTemp(temp); e != nil {
			return e
		}
		f, err = openFile(temp, os.O_WRONLY|os.O_TRUNC)
	}
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := rejectSymlink(path); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func openExistingTemp(path string) (bool, error) {
	f, err := openFile(path, os.O_RDONLY)
	if err != nil {
		return false, err
	}
	return true, f.Close()
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return ErrCorrupt
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func syncFile(path string) error {
	f, err := openFile(path, os.O_RDWR)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func newID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
