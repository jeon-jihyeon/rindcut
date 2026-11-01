package command

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"time"
)

func Read(ctx context.Context, p Prepared, req ReadRequest, definition Definition) (Page, error) {
	if !validReadRequest(req) {
		return Page{}, &Error{Code: "invalid_request"}
	}
	input, err := json.Marshal(req)
	if err != nil {
		return Page{}, &Error{Code: "invalid_request"}
	}
	timeout := definition.Reader.TimeoutSeconds
	if timeout == 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	r, err := Run(ctx, p, input)
	if err != nil {
		return Page{}, err
	}
	return DecodePage(r.Output, req, definition)
}

func DecodePage(data []byte, req ReadRequest, d Definition) (Page, error) {
	var page Page
	if !validReadRequest(req) {
		return page, &Error{Code: "invalid_request"}
	}
	if err := Decode(data, &page); err != nil {
		return Page{}, err
	}
	if !pageMatches(page, req, d) {
		return Page{}, &Error{Code: "invalid_response"}
	}
	if !validPageCursor(page, req) {
		return Page{}, &Error{Code: "invalid_response"}
	}
	for i := range page.Records {
		e := &page.Records[i]
		if !eventMatches(*e, req, d, *page.Generation) {
			return Page{}, &Error{Code: "invalid_response"}
		}
		for key := range e.Attributes {
			if !slices.Contains(d.Reader.AllowedAttributes, key) {
				return Page{}, &Error{Code: "invalid_response"}
			}
		}
		if e.OccurredAt != nil {
			t, _ := timeValue(*e.OccurredAt)
			value := Timestamp(t)
			e.OccurredAt = &value
		}
	}
	normalizeCoverage(&page.Coverage)
	return page, nil
}

func normalizeCoverage(coverage *Coverage) {
	for _, value := range []*string{coverage.WindowStart, coverage.WindowEnd} {
		if value == nil {
			continue
		}
		stamp, _ := timeValue(*value)
		*value = Timestamp(stamp)
	}
}

func pageMatches(p Page, r ReadRequest, d Definition) bool {
	return p.Protocol == SourceProtocol && p.SourceID == r.SourceID && p.SourceRevision == r.SourceRevision &&
		p.SourceInstance == r.SourceInstance && p.Stream == r.Stream && reflect.DeepEqual(p.Capability, d.Capability) &&
		validCoverage(p.Coverage) && len(p.Records) <= r.Limit
}

func eventMatches(e Event, r ReadRequest, d Definition, generation string) bool {
	return validEvent(e) && e.SourceInstance == r.SourceInstance && e.Stream == r.Stream && e.Generation == generation &&
		e.AdapterRevision == d.AdapterRevision && e.ProjectionProfile == d.ProjectionProfile
}

func validReadRequest(r ReadRequest) bool {
	if r.Protocol != SourceProtocol || r.Operation != "read" || r.SourceRevision < 1 ||
		!identifiers([]string{r.SourceID, r.SourceInstance, r.Stream}) || r.Limit < 1 || r.Limit > 100 {
		return false
	}
	if r.Generation != nil && !identifier(*r.Generation) {
		return false
	}
	return r.Cursor == nil || len(*r.Cursor) <= 8192
}

func validPageCursor(p Page, r ReadRequest) bool {
	if p.Generation != nil && !identifier(*p.Generation) {
		return false
	}
	if p.NextCursor != nil && len(*p.NextCursor) > 8192 {
		return false
	}
	if p.Status == "cursor_invalid" || p.Status == "unavailable" {
		return validUnavailablePage(p)
	}
	if p.Status != "ok" || p.Generation == nil {
		return false
	}
	if p.HasMore || len(p.Records) > 0 {
		return p.NextCursor != nil && (r.Cursor == nil || *p.NextCursor != *r.Cursor)
	}
	return p.NextCursor != nil || r.Cursor == nil
}

func validUnavailablePage(p Page) bool {
	if p.Status == "cursor_invalid" && p.Generation == nil {
		return false
	}
	if p.Status == "unavailable" && !slices.Contains(p.Coverage.Limitations, "unavailable") {
		return false
	}
	return len(p.Records) == 0 && p.NextCursor == nil && !p.HasMore
}
