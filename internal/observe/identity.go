package observe

type Identity struct {
	SourceID          string
	SourceInstance    string
	Generation        string
	Stream            string
	Type              string
	ID                string
	ProjectionProfile string
}

type Projection struct {
	Identity  Identity
	Digest    string
	CaptureID string
}

type Admission struct {
	Kind      string
	CaptureID string
	Conflicts []string
}

func (a Admission) Binding(captureRevision, sourceRevision, replayRevision int64) int64 {
	if a.Kind == "new" || a.Kind == "conflict" {
		return sourceRevision
	}
	if a.Kind != "replay" || replayRevision < 1 || captureRevision >= replayRevision {
		return 0
	}
	return replayRevision
}

func (a Admission) AllowsObserver(boundRevision, currentRevision int64) bool {
	return a.Kind != "replay" || boundRevision > 0 && boundRevision == currentRevision
}

func Admit(candidate Projection, existing []Projection) Admission {
	out := Admission{Kind: "new", Conflicts: []string{}}
	for _, prior := range existing {
		if prior.Identity != candidate.Identity {
			continue
		}
		if prior.Digest == candidate.Digest {
			out.Kind = "replay"
			out.CaptureID = prior.CaptureID
			continue
		}
		out.Conflicts = append(out.Conflicts, prior.CaptureID)
	}
	if out.Kind != "replay" && len(out.Conflicts) > 0 {
		out.Kind = "conflict"
	}
	return out
}
