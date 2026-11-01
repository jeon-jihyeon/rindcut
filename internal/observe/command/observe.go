package command

func (r ObserveRequest) Validate() error {
	if !validObserveRequest(r) {
		return &Error{Code: "invalid_request"}
	}
	return nil
}

func DecodeObservation(data []byte, r ObserveRequest) (ObservationResponse, error) {
	var out ObservationResponse
	if !validObserveRequest(r) {
		return out, &Error{Code: "invalid_request"}
	}
	if err := Decode(data, &out); err != nil {
		return ObservationResponse{}, err
	}
	if !observationMatches(out, r) || !ascii.MatchString(out.ReasonCode) || !validEvidence(out.Evidence) {
		return ObservationResponse{}, &Error{Code: "invalid_response"}
	}
	switch out.Result {
	case "confirmed", "refuted", "pending", "unknown":
	default:
		return ObservationResponse{}, &Error{Code: "invalid_response"}
	}
	if out.NextObserveAt != nil {
		t, ok := timeValue(*out.NextObserveAt)
		now, _ := timeValue(r.Now)
		if !ok || !t.After(now) {
			return ObservationResponse{}, &Error{Code: "invalid_response"}
		}
		value := Timestamp(t)
		out.NextObserveAt = &value
	}
	if out.Supersedes != nil && (out.Supersedes.StoreID != r.TargetRef.StoreID ||
		!identifier(out.Supersedes.ObservationID)) {
		return ObservationResponse{}, &Error{Code: "invalid_response"}
	}
	return out, nil
}

func observationMatches(out ObservationResponse, r ObserveRequest) bool {
	return out.Protocol == ObserverProtocol && out.SourceID == r.SourceID && out.SourceRevision == r.SourceRevision &&
		out.ObserverID == r.ObserverID && out.ObserverRevision == r.ObserverRevision && out.CriterionID == r.CriterionID &&
		out.CriterionRevision == r.CriterionRevision && out.SeriesID == r.SeriesID && out.TargetRef == r.TargetRef
}

func validObserveRequest(r ObserveRequest) bool {
	if r.Protocol != ObserverProtocol || r.SourceRevision < 1 || r.ObserverRevision < 1 ||
		!identifiers([]string{r.SourceID, r.ObserverID, r.CriterionID, r.CriterionRevision, r.SeriesID,
			r.TargetRef.StoreID, r.TargetRef.CaptureID}) || !attributes(r.Parameters) || !validEvent(r.Event.Event) {
		return false
	}
	if r.Event.SourceID != r.SourceID || r.Event.CaptureID != r.TargetRef.CaptureID || r.Event.SourceRevision < 1 ||
		!digest.MatchString(r.Event.NormalizedDigest) {
		return false
	}
	if _, ok := timeValue(r.Event.CapturedAt); !ok {
		return false
	}
	if _, ok := timeValue(r.Now); !ok {
		return false
	}
	return r.PreviousObservationID == nil || identifier(*r.PreviousObservationID)
}
