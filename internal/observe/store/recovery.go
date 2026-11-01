package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type recoveryIntent struct {
	Token      string `json:"token"`
	PreviousID string `json:"previous_store_id,omitempty"`
	NewID      string `json:"new_store_id"`
	Quarantine string `json:"quarantine"`
	Stage      string `json:"stage"`
}

func (s *Store) recoveryPath() string { return filepath.Join(s.home, "observe-recovery.json") }

func (s *Store) checkRecovery() error {
	var intent recoveryIntent
	if err := readJSON(s.recoveryPath(), &intent); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil || !validRecovery(intent) {
		return ErrIncomplete
	}
	return ErrRecoveryPending
}

func validRecovery(intent recoveryIntent) bool {
	if len(intent.Token) != 64 || !validStoreID(intent.NewID) ||
		intent.Quarantine != "store-"+intent.NewID {
		return false
	}
	if _, err := hex.DecodeString(intent.NewID); err != nil {
		return false
	}
	if _, err := hex.DecodeString(intent.Token); err != nil {
		return false
	}
	return intent.Stage == "planned" || intent.Stage == "quarantined" || intent.Stage == "initialized"
}

func (s *Store) RecoveryPreview(ctx context.Context) (Recovery, error) {
	unlock, present, err := s.readLock(ctx)
	if err != nil {
		return Recovery{}, err
	}
	if !present {
		return Recovery{Token: emptyToken(), Stage: "preview", ContinuityLost: true}, nil
	}
	defer unlock()
	return s.recoveryPreview(ctx)
}

func (s *Store) recoveryPreview(ctx context.Context) (Recovery, error) {
	var intent recoveryIntent
	if err := readJSON(s.recoveryPath(), &intent); err == nil {
		if !validRecovery(intent) {
			return Recovery{}, ErrIncomplete
		}
		return recoveryDTO(intent), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Recovery{}, ErrIncomplete
	}
	token, err := s.storeToken(ctx)
	if err != nil {
		return Recovery{}, err
	}
	var h head
	if err := readJSON(s.path("head.json"), &h); err != nil || !validStoreID(h.StoreID) {
		h = head{}
	}
	return Recovery{Token: token, Stage: "preview", StoreID: h.StoreID, ContinuityLost: true}, nil
}

func recoveryDTO(intent recoveryIntent) Recovery {
	return Recovery{
		Token: intent.Token, Stage: intent.Stage, StoreID: intent.PreviousID,
		NewStoreID: intent.NewID, Quarantine: filepath.Join("observe-quarantine", intent.Quarantine), ContinuityLost: true,
	}
}

func emptyToken() string {
	digest := sha256.Sum256([]byte("rindcut.store/v1:absent"))
	return hex.EncodeToString(digest[:])
}

func (s *Store) storeToken(ctx context.Context) (string, error) {
	if err := checkDir(s.path("")); errors.Is(err, os.ErrNotExist) {
		return emptyToken(), nil
	} else if err != nil {
		return "", ErrCorrupt
	}
	h := sha256.New()
	for _, name := range []string{"head.json", "ledger.jsonl", "commit.intent", "head.json.tmp", "commit.intent.tmp"} {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s\x00", name)
		f, err := openFile(s.path(name), os.O_RDONLY)
		if errors.Is(err, os.ErrNotExist) {
			_, _ = h.Write([]byte("absent\x00"))
			continue
		}
		if err != nil {
			return "", err
		}
		info, err := f.Stat()
		if err == nil {
			_, _ = fmt.Fprintf(h, "%d\x00", info.Size())
			_, err = io.Copy(h, &contextReader{ctx: ctx, reader: f})
		}
		closeErr := f.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

func (s *Store) Recover(ctx context.Context, expectedToken string) (Recovery, error) {
	if expectedToken == "" {
		return Recovery{}, ErrStalePreview
	}
	if err := ensureDir(s.home); err != nil {
		return Recovery{}, err
	}
	unlock, err := s.appendLock(ctx)
	if err != nil {
		return Recovery{}, err
	}
	defer unlock()
	preview, err := s.recoveryPreview(ctx)
	if err != nil {
		return Recovery{}, err
	}
	if preview.Token != expectedToken {
		return Recovery{}, ErrStalePreview
	}
	var intent recoveryIntent
	if preview.Stage == "preview" {
		id, err := newID()
		if err != nil {
			return Recovery{}, err
		}
		intent = recoveryIntent{
			Token: preview.Token, PreviousID: preview.StoreID, NewID: id, Quarantine: "store-" + id, Stage: "planned",
		}
		if err := writeJSON(s.recoveryPath(), intent); err != nil {
			return Recovery{}, err
		}
	} else if err := readJSON(s.recoveryPath(), &intent); err != nil {
		return Recovery{}, ErrIncomplete
	}
	if err := s.resumeRecovery(ctx, &intent); err != nil {
		return Recovery{}, err
	}
	result := recoveryDTO(intent)
	result.Stage = "complete"
	return result, nil
}

func (s *Store) resumeRecovery(ctx context.Context, intent *recoveryIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if intent.Stage == "planned" {
		if err := s.quarantine(*intent); err != nil {
			return err
		}
		intent.Stage = "quarantined"
		if err := writeJSON(s.recoveryPath(), intent); err != nil {
			return err
		}
	}
	if intent.Token != emptyToken() {
		path := filepath.Join(s.home, "observe-quarantine", intent.Quarantine)
		if err := checkDir(path); err != nil {
			return ErrIncomplete
		}
	}
	if intent.Stage == "quarantined" {
		if err := s.initializeRecovery(intent.NewID); err != nil {
			return err
		}
		intent.Stage = "initialized"
		if err := writeJSON(s.recoveryPath(), intent); err != nil {
			return err
		}
	}
	if err := s.verifyRecovered(ctx, intent.NewID); err != nil {
		return err
	}
	return removeSynced(s.recoveryPath())
}

func (s *Store) quarantine(intent recoveryIntent) error {
	root := filepath.Join(s.home, "observe-quarantine")
	if err := ensureDir(root); err != nil {
		return err
	}
	destination := filepath.Join(root, intent.Quarantine)
	_, sourceErr := os.Lstat(s.path(""))
	_, destinationErr := os.Lstat(destination)
	if errors.Is(sourceErr, os.ErrNotExist) {
		if destinationErr == nil {
			if err := checkDir(destination); err != nil {
				return err
			}
			if err := syncDir(root); err != nil {
				return err
			}
			return syncDir(s.home)
		}
		if errors.Is(destinationErr, os.ErrNotExist) && intent.PreviousID == "" && intent.Token == emptyToken() {
			return syncDir(s.home)
		}
		return ErrIncomplete
	}
	if sourceErr != nil || !errors.Is(destinationErr, os.ErrNotExist) {
		return ErrIncomplete
	}
	if err := checkDir(s.path("")); err != nil {
		return err
	}
	if err := os.Rename(s.path(""), destination); err != nil {
		return err
	}
	if err := syncDir(root); err != nil {
		return err
	}
	return syncDir(s.home)
}

func (s *Store) initializeRecovery(id string) error {
	if err := ensureDir(s.path("")); err != nil {
		return err
	}
	var h head
	err := readJSON(s.path("head.json"), &h)
	switch {
	case err == nil:
		if h.StoreID != id || h.Offset != 0 {
			return ErrIncomplete
		}
	case errors.Is(err, os.ErrNotExist):
		f, err := openFile(s.path("ledger.jsonl"), os.O_CREATE|os.O_RDWR)
		if err != nil {
			return err
		}
		info, statErr := f.Stat()
		if statErr != nil || info.Size() != 0 {
			_ = f.Close()
			return ErrIncomplete
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err := writeJSON(s.path("head.json"), head{StoreID: id}); err != nil {
			return err
		}
	default:
		return ErrIncomplete
	}
	return syncDir(s.home)
}

func (s *Store) verifyRecovered(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var h head
	if err := readJSON(s.path("head.json"), &h); err != nil || h.StoreID != id || h.Offset != 0 {
		return ErrIncomplete
	}
	f, err := openFile(s.path("ledger.jsonl"), os.O_RDONLY)
	if err != nil {
		return ErrIncomplete
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.Size() != 0 {
		return ErrIncomplete
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := syncFile(s.path("head.json")); err != nil {
		return err
	}
	if err := syncDir(s.path("")); err != nil {
		return err
	}
	return syncDir(s.home)
}
