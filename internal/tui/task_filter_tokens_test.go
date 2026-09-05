package tui

import (
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
	// A typed status token is the same token: F replaces it and keeps the rest.
	m.applyQuery(`-platform:twitch status:issues`)
	m.CycleFilter() // issues → finished
	if m.Query() != "-platform:twitch status:finished" {
		t.Fatalf("F must replace the typed status token in place: %q", m.Query())
	}
	if !strings.Contains(stripANSI(m.renderHeader(100)), "[-platform:twitch status:finished]") {
		t.Fatalf("header must show the serialized query: %q", stripANSI(m.renderHeader(100)))
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
