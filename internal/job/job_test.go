package job_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/check"
	"github.com/jeon-jihyeon/rindcut/internal/job"
)

func valid() job.Job {
	return job.Job{
		ID: "price-watch", Name: "가격 수집", Schedule: "0 9 * * *", Timezone: "Asia/Seoul",
		Grace: check.Duration(30 * time.Minute), Evidence: job.EvidenceSession,
		Folder: "/Users/owner/price", Marker: "오늘 가격 수집해줘",
		Checks:   []check.Spec{{Kind: check.KindFile, Path: "out/{{date}}.csv"}},
		Revision: 1,
	}
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cloud := func(j *job.Job) { j.Evidence, j.Folder, j.Marker = job.EvidenceNone, "", "" }
	tcs := []struct {
		name string
		edit func(*job.Job)
		bad  string
	}{
		{name: "valid", edit: func(*job.Job) {}},
		{name: "cloud job needs no folder", edit: func(j *job.Job) { cloud(j); j.Checks[0].Path = "~/out/{{date}}.csv" }},
		{name: "cloud job with a relative path", edit: cloud, bad: "must start with / or ~/"},
		{name: "removed needs only identity", edit: func(j *job.Job) { j.Removed, j.Schedule, j.Checks = true, "", nil }},
		{name: "upper case id", edit: func(j *job.Job) { j.ID = "Price" }, bad: "id"},
		{name: "long id", edit: func(j *job.Job) { j.ID = strings.Repeat("a", 49) }, bad: "id"},
		{name: "empty name", edit: func(j *job.Job) { j.Name = " " }, bad: "name"},
		{name: "revision 0", edit: func(j *job.Job) { j.Revision = 0 }, bad: "revision"},
		{name: "six field cron", edit: func(j *job.Job) { j.Schedule = "0 0 9 * * *" }, bad: "5 fields"},
		{name: "bad hour", edit: func(j *job.Job) { j.Schedule = "0 24 * * *" }, bad: "hour"},
		{name: "zero step", edit: func(j *job.Job) { j.Schedule = "*/0 * * * *" }, bad: "step"},
		{name: "backwards range", edit: func(j *job.Job) { j.Schedule = "0 9 * * fri-mon" }, bad: "backwards"},
		{name: "bad zone", edit: func(j *job.Job) { j.Timezone = "Seoul" }, bad: "timezone"},
		{name: "empty zone", edit: func(j *job.Job) { j.Timezone = "" }, bad: "timezone"},
		{name: "short grace", edit: func(j *job.Job) { j.Grace = check.Duration(time.Second) }, bad: "grace"},
		{name: "long grace", edit: func(j *job.Job) { j.Grace = check.Duration(25 * time.Hour) }, bad: "grace"},
		{name: "unknown evidence", edit: func(j *job.Job) { j.Evidence = "cloud" }, bad: "evidence"},
		{name: "relative folder", edit: func(j *job.Job) { j.Folder = "price" }, bad: "folder"},
		{name: "no marker", edit: func(j *job.Job) { j.Marker = "" }, bad: "marker"},
		{name: "two line marker", edit: func(j *job.Job) { j.Marker = "a\nb" }, bad: "marker"},
		{name: "200 korean characters fit", edit: func(j *job.Job) { j.Marker = strings.Repeat("가", 200) }},
		{name: "201 characters don't", edit: func(j *job.Job) { j.Marker = strings.Repeat("가", 201) }, bad: "marker"},
		{name: "no checks", edit: func(j *job.Job) { j.Checks = nil }, bad: "checks"},
		{name: "eleven checks", edit: func(j *job.Job) { j.Checks = make([]check.Spec, 11) }, bad: "checks"},
		{name: "bad check", edit: func(j *job.Job) { j.Checks = []check.Spec{{Kind: check.KindFile}} }, bad: "check 1"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := valid()
			j.Checks = append([]check.Spec(nil), j.Checks...)
			tc.edit(&j)
			err := j.Validate()
			if tc.bad == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.bad)
		})
	}
}

func TestSlug(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name, in, want string
	}{
		{name: "words", in: "Price Watch", want: "price-watch"},
		{name: "symbols collapse", in: "  weekly -- report! ", want: "weekly-report"},
		{name: "korean dropped", in: "주간 report", want: "report"},
		{name: "cut at 48", in: strings.Repeat("ab", 40), want: strings.Repeat("ab", 24)},
		{name: "korean only hashes", in: "가격 수집", want: job.Slug(" 가격 수집 ")},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, job.Slug(tc.in))
		})
	}
	assert.Regexp(t, `^job-[0-9a-f]{8}$`, job.Slug("가격 수집"))
}

func slotTimes(t *testing.T, j job.Job, from, to time.Time) []string {
	t.Helper()
	slots, err := j.Slots(from, to)
	require.NoError(t, err)
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.At.Format(time.RFC3339)
	}
	return out
}

func TestSlotsCount(t *testing.T) {
	t.Parallel()
	j := valid()
	seoul := zone(t, "Asia/Seoul")
	tcs := []struct {
		name  string
		year  int
		count int
	}{
		{name: "2026", year: 2026, count: 365},
		{name: "leap 2028", year: 2028, count: 366},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			from := time.Date(tc.year, 1, 1, 0, 0, 0, 0, seoul)
			got := slotTimes(t, j, from, from.AddDate(1, 0, 0))
			require.Len(t, got, tc.count)
			assert.Equal(t, from.Add(9*time.Hour).Format(time.RFC3339), got[0])
		})
	}
}

func TestSlotsCron(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) // a Thursday
	tcs := []struct {
		name, schedule string
		to             time.Time
		want           []string
	}{
		{name: "step", schedule: "*/20 9 * * *", to: from.AddDate(0, 0, 1),
			want: []string{"2026-10-01T09:00:00Z", "2026-10-01T09:20:00Z", "2026-10-01T09:40:00Z"}},
		{name: "weekday names", schedule: "0 9 * * mon-fri", to: from.AddDate(0, 0, 5),
			want: []string{"2026-10-01T09:00:00Z", "2026-10-02T09:00:00Z", "2026-10-05T09:00:00Z"}},
		{name: "7 is sunday", schedule: "0 9 * * 7", to: from.AddDate(0, 0, 7), want: []string{"2026-10-04T09:00:00Z"}},
		{name: "31st only in long months", schedule: "30 8 31 * *", to: from.AddDate(0, 2, 0), want: []string{"2026-10-31T08:30:00Z"}},
		{name: "both day fields match either", schedule: "0 0 1 * fri", to: from.AddDate(0, 0, 10),
			want: []string{"2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", "2026-10-09T00:00:00Z"}},
		{name: "lists, ranges and month names", schedule: "15,45 6-7 * jan *", to: time.Date(2027, 1, 1, 7, 0, 0, 0, time.UTC),
			want: []string{"2027-01-01T06:15:00Z", "2027-01-01T06:45:00Z"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := valid()
			j.Timezone, j.Schedule = "UTC", tc.schedule
			assert.Equal(t, tc.want, slotTimes(t, j, from, tc.to))
		})
	}
}

func TestSlotsDST(t *testing.T) {
	t.Parallel()
	ny := zone(t, "America/New_York")
	tcs := []struct {
		name, schedule string
		from, to       time.Time
		want           []string
	}{
		// 2026-03-08 skips 02:00 to 03:00
		{name: "gap day has no slot", schedule: "30 2 * * *",
			from: time.Date(2026, 3, 7, 0, 0, 0, 0, ny), to: time.Date(2026, 3, 10, 0, 0, 0, 0, ny),
			want: []string{"2026-03-07T02:30:00-05:00", "2026-03-09T02:30:00-04:00"}},
		// 2026-11-01 repeats 01:00 to 02:00
		{name: "overlap yields the first time only", schedule: "30 1 * * *",
			from: time.Date(2026, 11, 1, 0, 0, 0, 0, ny), to: time.Date(2026, 11, 2, 0, 0, 0, 0, ny),
			want: []string{"2026-11-01T01:30:00-04:00"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := valid()
			j.Timezone, j.Schedule = "America/New_York", tc.schedule
			assert.Equal(t, tc.want, slotTimes(t, j, tc.from, tc.to))
		})
	}
}

func TestSlotWindowAndKey(t *testing.T) {
	t.Parallel()
	j := valid()
	from := time.Date(2026, 10, 8, 0, 0, 0, 0, zone(t, "Asia/Seoul"))
	slots, err := j.Slots(from, from.AddDate(0, 0, 1))
	require.NoError(t, err)
	require.Len(t, slots, 1)
	assert.Equal(t, 30*time.Minute, slots[0].End.Sub(slots[0].At))
	assert.Equal(t, "price-watch@2026-10-08T09:00:00+09:00", slots[0].Key())
}

func TestRevisions(t *testing.T) {
	t.Parallel()
	r1 := valid()
	r1.Since = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	r2 := r1
	r2.Revision, r2.Grace, r2.Since = 2, check.Duration(time.Hour), time.Date(2026, 10, 8, 0, 10, 0, 0, time.UTC)
	r3 := r2
	r3.Revision, r3.Removed, r3.Since = 3, true, time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	revs := []job.Job{r2, r1, r3}

	cur := job.Current(revs)["price-watch"]
	assert.Equal(t, 3, cur.Revision)
	assert.True(t, cur.Removed)

	type want struct {
		found    bool
		revision int
	}
	tcs := []struct {
		name string
		at   time.Time
		want want
	}{
		{name: "before the job existed", at: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), want: want{}},
		{name: "first revision", at: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), want: want{found: true, revision: 1}},
		{name: "at the change", at: time.Date(2026, 10, 8, 0, 10, 0, 0, time.UTC), want: want{found: true, revision: 2}},
		{name: "after removal", at: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), want: want{found: true, revision: 3}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := job.At(revs, "price-watch", tc.at)
			assert.Equal(t, tc.want, want{found: ok, revision: got.Revision})
		})
	}
}
