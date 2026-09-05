package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func tokenFixtureJobs() []*database.Job {
	return []*database.Job{
		{ID: "1", Title: "Hololive concert", ChannelName: "Shachi Too", Status: database.StatusLive, Platform: "youtube", VideoID: "aaaaaaaaaaa"},
		{ID: "2", Title: "Random clip", ChannelName: "Shachi Too", Status: database.StatusFinished, Platform: "youtube", VideoID: "bbbbbbbbbbb"},
		{ID: "3", Title: "Stream VOD", ChannelName: "Another Ch", Status: database.StatusError, Platform: "twitch", VideoID: "tw_v123"},
		{ID: "4", Title: "Upcoming debut", ChannelName: "Debut Channel", Status: database.StatusUpcoming, Platform: "youtube", VideoID: "ccccccccccc"},
	}
}

func visibleIDs(m *TaskListModel) []string {
	var ids []string
	for _, it := range m.list.Items() {
		if ti, ok := it.(taskItem); ok && ti.job != nil {
			ids = append(ids, ti.job.ID)
		}
	}
	return ids
}

// TestFCyclesTheStatusToken: F walks none → active → issues → finished → none,
// replacing any status token the operator typed, and the header shows the
// serialized query.
func TestFCyclesTheStatusToken(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(100, 30)
	m.SetJobs(tokenFixtureJobs())
	if got := m.Query(); got != "" {
		t.Fatalf("initial query %q", got)
	}
	m.CycleFilter()
	if m.Query() != "status:active" || !equalIDs(visibleIDs(m), []string{"1", "4"}) {
		t.Fatalf("after F: query %q ids %v", m.Query(), visibleIDs(m))
	}
	m.CycleFilter()
	if m.Query() != "status:issues" || !equalIDs(visibleIDs(m), []string{"3"}) {
		t.Fatalf("after F F: %q %v", m.Query(), visibleIDs(m))
	}
	m.CycleFilter()
	if m.Query() != "status:finished" || !equalIDs(visibleIDs(m), []string{"2"}) {
		t.Fatalf("after F F F: %q %v", m.Query(), visibleIDs(m))
	}
	m.CycleFilter()
	if m.Query() != "" || len(visibleIDs(m)) != 4 {
		t.Fatalf("fourth F must clear: %q %v", m.Query(), visibleIDs(m))
	}
	// A typed status token is the same token: F replaces it IN PLACE and
	// keeps the rest. The status token leads here on purpose — with it last,
	// an implementation that appended the replacement instead of inserting
	// at the vacated index would serialize identically and the assertion
	// would prove nothing.
	m.applyQuery(`status:issues -platform:twitch`)
	m.CycleFilter() // issues → finished
	if m.Query() != "status:finished -platform:twitch" {
		t.Fatalf("F must replace the typed status token in place: %q", m.Query())
	}
	// 32 columns, inside renderHeader's max(w/3, 12) = 33 budget, so the
	// indicator is shown whole here — the ellipsizing is pinned separately
	// by TestHeaderEllipsizesALongQuery.
	if !strings.Contains(stripANSI(m.renderHeader(100)), "[status:finished -platform:twitch]") {
		t.Fatalf("header must show the serialized query: %q", stripANSI(m.renderHeader(100)))
	}

	// H3's wide rule: a raw status value is F's token too. Keeping it beside
	// the new one would AND two disjoint status sets and show nothing, so F
	// replaces it rather than adding to it.
	m.applyQuery("status:live")
	m.CycleFilter()
	if m.Query() != "status:active" {
		t.Fatalf("F must replace a raw status token, not keep it: %q", m.Query())
	}
}

// TestHeaderEllipsizesALongQuery: the indicator is held to a third of the
// panel so a long query cannot evict the position range on the right.
func TestHeaderEllipsizesALongQuery(t *testing.T) {
	const channel = "some-very-long-channel-name-here"
	jobs := make([]*database.Job, 0, 10)
	for i := range 10 {
		jobs = append(jobs, &database.Job{
			ID:          string(rune('a' + i)),
			Title:       "Archive run",
			ChannelName: channel,
			Status:      database.StatusFinished,
			Platform:    "youtube",
		})
	}
	m := NewTaskListModel()
	m.SetSize(60, 8) // contentHeight 5 < 10 rows, so a position range renders
	m.SetJobs(jobs)
	m.applyQuery("status:finished channel:" + channel + " platform:youtube")
	if got := len(visibleIDs(m)); got != 10 {
		t.Fatalf("query should match all 10 jobs, matched %d", got)
	}

	hdr := stripANSI(m.renderHeader(60))
	if !regexp.MustCompile(`\[\d+-\d+/10\]`).MatchString(hdr) {
		t.Errorf("the position range must survive a long query: %q", hdr)
	}
	if !strings.Contains(hdr, "…]") {
		t.Errorf("a query wider than w/3 must be ellipsized: %q", hdr)
	}
	if strings.Contains(hdr, "platform:youtube") {
		t.Errorf("the full query must not be rendered at width 60: %q", hdr)
	}
}

// TestTypedTokensFilterTheList: the / box understands the whole language.
func TestTypedTokensFilterTheList(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(100, 30)
	m.SetJobs(tokenFixtureJobs())
	for query, want := range map[string][]string{
		`shachi`:                          {"1", "2"},
		`channel:"shachi too" -clip`:      {"1"},
		`platform:twitch|status:upcoming`: {"3", "4"},
		`tw_v`:                            {"3"}, // video-ID substring
		`status:live`:                     {"1"}, // raw status fallback
		`nothing-matches`:                 nil,
	} {
		m.applyQuery(query)
		if got := visibleIDs(m); !equalIDs(got, want) {
			t.Errorf("%q → %v, want %v", query, got, want)
		}
	}
}

// TestArchiveVisibilityFollowsTheStatusToken: the archive section is hidden
// while a status token excludes Finished; a negated token does not hide it.
func TestArchiveVisibilityFollowsTheStatusToken(t *testing.T) {
	m := NewTaskListModel()
	m.applyQuery("status:active")
	if m.showArchive() {
		t.Error("status:active must hide the archive")
	}
	m.applyQuery("status:finished")
	if !m.showArchive() {
		t.Error("status:finished must show the archive")
	}
	m.applyQuery("status:live")
	if m.showArchive() {
		t.Error("a raw status name other than Finished must hide the archive")
	}
	m.applyQuery("-status:active")
	if !m.showArchive() {
		t.Error("a negated status token must not hide the archive")
	}
	m.applyQuery("channel:x")
	if !m.showArchive() {
		t.Error("no status token → archive shown")
	}
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
