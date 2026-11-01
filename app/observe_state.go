package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jeon-jihyeon/rindcut/internal/observe"
	"github.com/jeon-jihyeon/rindcut/internal/observe/command"
	obsstore "github.com/jeon-jihyeon/rindcut/internal/observe/store"
)

type observeSource struct {
	SourceID          string                      `json:"source_id"`
	Revision          int64                       `json:"revision"`
	Definition        command.Definition          `json:"definition"`
	Reader            command.Prepared            `json:"reader"`
	Observers         map[string]command.Prepared `json:"observers"`
	ObserverRevisions map[string]int64            `json:"observer_revisions"`
	Collection        string                      `json:"collection_identity"`
	Removed           bool                        `json:"removed"`
}

type observeCheckpoint struct {
	SourceID             string           `json:"source_id"`
	SourceRevision       int64            `json:"source_revision"`
	Collection           string           `json:"collection_identity"`
	Generation           *string          `json:"generation" nullable:"true"`
	Cursor               *string          `json:"cursor" nullable:"true"`
	Blocked              string           `json:"blocked,omitempty"`
	Gap                  bool             `json:"gap"`
	Coverage             command.Coverage `json:"coverage"`
	LastSuccessAt        *time.Time       `json:"last_success_at,omitempty"`
	ReplaySourceRevision int64            `json:"replay_source_revision,omitempty"`
}

type observeSeries struct {
	ID                    string    `json:"id"`
	StoreID               string    `json:"store_id"`
	SourceID              string    `json:"source_id"`
	SourceRevision        int64     `json:"source_revision"`
	CaptureSourceRevision int64     `json:"capture_source_revision"`
	CaptureID             string    `json:"capture_id"`
	ObserverID            string    `json:"observer_id"`
	ObserverRevision      int64     `json:"observer_revision"`
	CriterionID           string    `json:"criterion_id"`
	CriterionRevision     string    `json:"criterion_revision"`
	ParametersDigest      string    `json:"parameters_digest"`
	ActorKind             string    `json:"actor_kind"`
	Mode                  string    `json:"mode"`
	IntervalSeconds       int       `json:"interval_seconds"`
	DeadlineSeconds       int       `json:"deadline_seconds"`
	CapturedAt            time.Time `json:"captured_at"`
	Status                string    `json:"status"`
	DueAt                 time.Time `json:"due_at"`
	LastObservationID     string    `json:"last_observation_id,omitempty"`
}

type observeRotation struct {
	NextLane      string `json:"next_lane"`
	AfterSourceID string `json:"after_source_id"`
}

type observeProgress struct {
	Checkpoint *observeCheckpoint `json:"checkpoint,omitempty"`
	Series     *observeSeries     `json:"series,omitempty"`
	Rotation   *observeRotation   `json:"rotation,omitempty"`
}

type observeObservation struct {
	command.ObservationResponse
	ObservationID         string    `json:"observation_id"`
	ObservedAt            time.Time `json:"observed_at"`
	CaptureSourceRevision int64     `json:"capture_source_revision"`
	ActorKind             string    `json:"actor_kind"`
	Type                  string    `json:"type"`
	ActionID              *string   `json:"action_id,omitempty"`
}

type observeAttempt struct {
	AttemptID        string `json:"attempt_id"`
	CycleID          string `json:"cycle_id"`
	SourceID         string `json:"source_id"`
	SeriesID         string `json:"series_id,omitempty"`
	Stage            string `json:"stage"`
	Started          bool   `json:"started"`
	Exited           bool   `json:"exited"`
	ReasonCode       string `json:"reason_code,omitempty"`
	SourceRevision   int64  `json:"source_revision"`
	ObserverID       string `json:"observer_id,omitempty"`
	ObserverRevision int64  `json:"observer_revision,omitempty"`
}

type observeSeriesKey struct {
	CaptureID        string
	SourceRevision   int64
	ObserverID       string
	ObserverRevision int64
}

type observeSourceKey struct {
	SourceID string
	Revision int64
}

type observeState struct {
	snapshot        obsstore.Snapshot
	sources         map[string]observeSource
	sourceRevisions map[observeSourceKey]observeSource
	checkpoints     map[string]observeCheckpoint
	captures        map[string]command.Capture
	series          map[string]observeSeries
	observations    map[string]observeObservation
	attempts        map[string]observeAttempt
	rotation        observeRotation
	projections     map[observe.Identity][]observe.Projection
	seriesKeys      map[observeSeriesKey]string
	observationIDs  map[string][]string
	lastReason      map[string]string
	lastAttempt     map[string]time.Time
	models          map[string]observe.Series
}

func loadObserve(ctx context.Context, snapshot obsstore.Snapshot) (observeState, error) {
	s := observeState{snapshot: snapshot, sources: map[string]observeSource{},
		checkpoints: map[string]observeCheckpoint{}, captures: map[string]command.Capture{},
		series: map[string]observeSeries{}, observations: map[string]observeObservation{},
		attempts: map[string]observeAttempt{}, projections: map[observe.Identity][]observe.Projection{},
		seriesKeys: map[observeSeriesKey]string{}, observationIDs: map[string][]string{}, lastReason: map[string]string{},
		lastAttempt: map[string]time.Time{}, sourceRevisions: map[observeSourceKey]observeSource{},
		models: map[string]observe.Series{}}
	for _, r := range snapshot.Records {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		if err := s.accept(r); err != nil {
			return s, errors.New("store_corrupt")
		}
	}
	if err := s.validate(ctx); err != nil {
		if ctx.Err() != nil {
			return s, ctx.Err()
		}
		return s, errors.New("store_corrupt")
	}
	s.snapshot.Records = nil
	return s, nil
}

func (s *observeState) accept(r obsstore.Record) error {
	switch r.Kind {
	case "source":
		return s.acceptSource(r.Data)
	case "event":
		return s.acceptCapture(r)
	case "observation":
		return s.acceptObservation(r)
	case "checkpoint":
		return s.acceptProgress(r.Data)
	case "cycle":
		return s.acceptCycle(r)
	default:
		return errors.New("invalid_kind")
	}
}

func (s *observeState) acceptSource(data []byte) error {
	var source observeSource
	if err := command.Decode(data, &source); err != nil || source.SourceID != source.Definition.ID ||
		source.Revision != s.sources[source.SourceID].Revision+1 || source.Definition.Validate() != nil ||
		!observeDigest(source.Collection) || !validObservePrepared(source.Reader, source.Definition.Reader.Args) {
		return errors.New("invalid_source")
	}
	if s.sources[source.SourceID].Removed || source.Removed && source.Definition.Enabled ||
		len(source.Observers) != len(source.Definition.Observers) {
		return errors.New("invalid_source")
	}
	for _, observer := range source.Definition.Observers {
		if source.ObserverRevisions[observer.ID] < 1 || !validObservePrepared(source.Observers[observer.ID], observer.Args) {
			return errors.New("invalid_source")
		}
	}
	if !validObserveRevisions(source.ObserverRevisions) {
		return errors.New("invalid_source")
	}
	s.sources[source.SourceID] = source
	s.sourceRevisions[observeSourceKey{source.SourceID, source.Revision}] = source
	return nil
}

func validObserveRevisions(revisions map[string]int64) bool {
	for _, revision := range revisions {
		if revision < 1 {
			return false
		}
	}
	return true
}

func validObservePrepared(prepared command.Prepared, args []string) bool {
	if !filepath.IsAbs(prepared.Program) || !filepath.IsAbs(prepared.Dir) ||
		!slices.Equal(prepared.Args, args) || len(prepared.Fingerprints) < 1 || len(prepared.Fingerprints) > 17 ||
		prepared.Fingerprints[0].Path != prepared.Program {
		return false
	}
	for _, fingerprint := range prepared.Fingerprints {
		if !filepath.IsAbs(fingerprint.Path) || !observeDigest(fingerprint.Digest) {
			return false
		}
	}
	return true
}

func observeDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	for _, char := range value[7:] {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func (s *observeState) acceptCapture(r obsstore.Record) error {
	var capture command.Capture
	if err := command.Decode(r.Data, &capture); err != nil || capture.CaptureID != r.ID {
		return errors.New("invalid_capture")
	}
	source, ok := s.sourceRevisions[observeSourceKey{capture.SourceID, capture.SourceRevision}]
	if !ok || capture.SourceInstance != source.Definition.SourceInstance || capture.Stream != source.Definition.Stream ||
		capture.AdapterRevision != source.Definition.AdapterRevision ||
		capture.ProjectionProfile != source.Definition.ProjectionProfile {
		return errors.New("invalid_capture")
	}
	_, digest, err := command.Projection(capture.Event)
	if err != nil || digest != capture.NormalizedDigest {
		return errors.New("invalid_capture")
	}
	if _, err := time.Parse(time.RFC3339Nano, capture.CapturedAt); err != nil {
		return errors.New("invalid_capture")
	}
	for key := range capture.Attributes {
		if !slices.Contains(source.Definition.Reader.AllowedAttributes, key) {
			return errors.New("invalid_capture")
		}
	}
	if _, exists := s.captures[capture.CaptureID]; exists {
		return errors.New("invalid_capture")
	}
	s.captures[capture.CaptureID] = capture
	p := captureProjection(capture)
	s.projections[p.Identity] = append(s.projections[p.Identity], p)
	return nil
}

func (s *observeState) acceptObservation(r obsstore.Record) error {
	var observation observeObservation
	if err := command.Decode(r.Data, &observation); err != nil || observation.ObservationID != r.ID {
		return errors.New("invalid_observation")
	}
	series, exists := s.series[observation.SeriesID]
	if !exists || !observeObservationMatches(observation, series) || !s.validObservationPayload(observation, series) {
		return errors.New("invalid_observation")
	}
	if _, duplicate := s.observations[r.ID]; duplicate {
		return errors.New("invalid_observation")
	}
	if observation.Supersedes != nil {
		prior, exists := s.observations[observation.Supersedes.ObservationID]
		if !exists || prior.SeriesID != observation.SeriesID || observation.Supersedes.StoreID != series.StoreID {
			return errors.New("invalid_observation")
		}
	}
	s.observations[r.ID] = observation
	s.observationIDs[observation.SeriesID] = append(s.observationIDs[observation.SeriesID], r.ID)
	return nil
}

func (s *observeState) acceptProgress(data []byte) error {
	var progress observeProgress
	if err := command.Decode(data, &progress); err != nil {
		return err
	}
	count := 0
	if progress.Checkpoint != nil {
		count++
	}
	if progress.Series != nil {
		count++
	}
	if progress.Rotation != nil {
		count++
	}
	if count != 1 {
		return errors.New("invalid_checkpoint")
	}
	if progress.Checkpoint != nil {
		return s.acceptCheckpoint(*progress.Checkpoint)
	}
	if progress.Series != nil {
		return s.acceptSeries(*progress.Series)
	}
	rotation := *progress.Rotation
	if rotation.NextLane != "source" && rotation.NextLane != "observation" {
		return errors.New("invalid_checkpoint")
	}
	s.rotation = rotation
	return nil
}

func (s *observeState) acceptCheckpoint(checkpoint observeCheckpoint) error {
	source, exists := s.sourceRevisions[observeSourceKey{checkpoint.SourceID, checkpoint.SourceRevision}]
	if !exists || !observeDigest(checkpoint.Collection) || checkpoint.Cursor != nil && checkpoint.Generation == nil ||
		checkpoint.Cursor != nil && len(*checkpoint.Cursor) > 8192 {
		return errors.New("invalid_checkpoint")
	}
	if checkpoint.Collection != source.Collection && checkpoint.Blocked != "collection_changed" {
		return errors.New("invalid_checkpoint")
	}
	if !s.validReplayCheckpoint(checkpoint, source) {
		return errors.New("invalid_checkpoint")
	}
	switch checkpoint.Coverage.Kind {
	case "unknown", "event_stream", "snapshot":
	default:
		return errors.New("invalid_checkpoint")
	}
	s.checkpoints[checkpoint.SourceID] = checkpoint
	return nil
}

func (s observeState) validReplayCheckpoint(checkpoint observeCheckpoint, source observeSource) bool {
	if checkpoint.ReplaySourceRevision == 0 {
		return true
	}
	if checkpoint.ReplaySourceRevision < 0 || checkpoint.ReplaySourceRevision > source.Revision ||
		!source.Definition.Enabled || source.Removed {
		return false
	}
	bound, exists := s.sourceRevisions[observeSourceKey{checkpoint.SourceID, checkpoint.ReplaySourceRevision}]
	return exists && bound.Collection == checkpoint.Collection && bound.Definition.Enabled && !bound.Removed
}

func (s *observeState) acceptCycle(r obsstore.Record) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &fields); err != nil {
		return err
	}
	if _, ok := fields["attempt_id"]; ok {
		return s.acceptAttempt(r)
	}
	var cycle ObserveCycle
	if err := command.Decode(r.Data, &cycle); err != nil || cycle.ID == "" || cycle.StartedAt.IsZero() ||
		cycle.FinishedAt.IsZero() || cycle.Attempts < 0 || cycle.Attempts > 100 || cycle.Captured < 0 || cycle.Observations < 0 {
		return errors.New("invalid_cycle")
	}
	return nil
}

func (s *observeState) acceptAttempt(r obsstore.Record) error {
	var attempt observeAttempt
	if err := command.Decode(r.Data, &attempt); err != nil || attempt.AttemptID == "" || attempt.CycleID == "" ||
		attempt.Exited && !attempt.Started {
		return errors.New("invalid_attempt")
	}
	if _, exists := s.sourceRevisions[observeSourceKey{attempt.SourceID, attempt.SourceRevision}]; !exists {
		return errors.New("invalid_attempt")
	}
	prior, exists := s.attempts[attempt.AttemptID]
	switch attempt.Stage {
	case "intent":
		if exists || attempt.Started || attempt.Exited {
			return errors.New("invalid_attempt")
		}
		s.lastAttempt[attempt.SourceID] = r.At
	case "complete":
		if !exists || prior.Stage != "intent" || !observeAttemptMatches(prior, attempt) {
			return errors.New("invalid_attempt")
		}
		s.lastReason[attempt.SourceID] = attempt.ReasonCode
	default:
		return errors.New("invalid_attempt")
	}
	s.attempts[attempt.AttemptID] = attempt
	return nil
}

func observeAttemptMatches(prior, attempt observeAttempt) bool {
	return prior.CycleID == attempt.CycleID && prior.SourceID == attempt.SourceID &&
		prior.SourceRevision == attempt.SourceRevision && prior.SeriesID == attempt.SeriesID &&
		prior.ObserverID == attempt.ObserverID && prior.ObserverRevision == attempt.ObserverRevision
}

func (s *observeState) validate(ctx context.Context) error {
	for id, source := range s.sources {
		checkpoint, exists := s.checkpoints[id]
		if !exists || checkpoint.SourceRevision != source.Revision {
			return errors.New("invalid_checkpoint")
		}
	}
	for _, saved := range s.series {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.validateSeries(ctx, saved); err != nil {
			return err
		}
	}
	return nil
}

func captureProjection(c command.Capture) observe.Projection {
	return observe.Projection{Identity: observe.Identity{SourceID: c.SourceID,
		SourceInstance: c.SourceInstance, Generation: c.Generation, Stream: c.Stream,
		Type: c.Type, ID: c.ID, ProjectionProfile: c.ProjectionProfile},
		Digest: c.NormalizedDigest, CaptureID: c.CaptureID}
}

func observeHash(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func observeRecord(kind string, at time.Time, value any) (obsstore.Record, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return obsstore.Record{}, errors.New("invalid_record")
	}
	return obsstore.Record{ID: rand.Text(), Kind: kind, At: at.UTC(), Data: b}, nil
}

func observeCode(err error) string {
	var failure *command.Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	for _, item := range []struct {
		err  error
		code string
	}{{obsstore.ErrBusy, "busy"}, {obsstore.ErrCorrupt, "store_corrupt"},
		{obsstore.ErrIncomplete, "storage_incomplete"}, {obsstore.ErrRecoveryPending, "recovery_pending"},
		{obsstore.ErrInvalidCursor, "invalid_cursor"}, {obsstore.ErrStalePreview, "stale_preview"},
		{obsstore.ErrLineTooLong, "record_too_large"}} {
		if errors.Is(err, item.err) {
			return item.code
		}
	}
	return "store_unavailable"
}

func (s observeState) sourceIDs() []string {
	ids := make([]string, 0, len(s.sources))
	for id := range s.sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
