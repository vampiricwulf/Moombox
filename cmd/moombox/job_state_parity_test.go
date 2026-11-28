package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/tui"
)

// jobStateParityFile is the table both implementations of the derived job
// sub-states run against: internal/jobstate here, web/public/modules/utils.js
// in web/tests/job-substates.test.mjs.
const jobStateParityFile = "../../web/tests/fixtures/job-state-parity.json"

type jobStateParity struct {
	PendingAutoAction []struct {
		Name string          `json:"name"`
		Job  json.RawMessage `json:"job"`
		Want string          `json:"want"`
	} `json:"pendingAutoAction"`
	WaitLines []struct {
		Line string `json:"line"`
		Want bool   `json:"want"`
	} `json:"waitLines"`
	AwaitingProcessing []struct {
		Name string          `json:"name"`
		Job  json.RawMessage `json:"job"`
		Want bool            `json:"want"`
	} `json:"awaitingProcessing"`
	QueuedHolds []struct {
		Name  string                  `json:"name"`
		Holds jobstate.AdmissionHolds `json:"holds"`
		Job   database.Job            `json:"job"`
		Now   time.Time               `json:"now"`
		Want  struct {
			Reason  string    `json:"reason"`
			Until   time.Time `json:"until"`
			Attempt int       `json:"attempt"`
			Of      int       `json:"of"`
		} `json:"want"`
	} `json:"queuedHolds"`
	CookieRetries []struct {
		Name  string                  `json:"name"`
		Holds jobstate.AdmissionHolds `json:"holds"`
		Job   database.Job            `json:"job"`
		Now   time.Time               `json:"now"`
		Want  jobstate.CookieRetry    `json:"want"`
	} `json:"cookieRetries"`
	SuggestedChatOffset []struct {
		Name string          `json:"name"`
		Job  json.RawMessage `json:"job"`
		Want struct {
			Seconds int    `json:"seconds"`
			Known   bool   `json:"known"`
			Basis   string `json:"basis"`
			Line    string `json:"line"`
		} `json:"want"`
	} `json:"suggestedChatOffset"`
	OperatorChatOffset []struct {
		Name string          `json:"name"`
		Job  json.RawMessage `json:"job"`
		Want bool            `json:"want"`
	} `json:"operatorChatOffset"`
}

func loadJobStateParity(t *testing.T) jobStateParity {
	t.Helper()
	raw, err := os.ReadFile(jobStateParityFile)
	if err != nil {
		t.Fatalf("read %s: %v", jobStateParityFile, err)
	}
	var p jobStateParity
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("parse %s: %v", jobStateParityFile, err)
	}
	return p
}

// TestPendingAutoActionParity runs jobstate.PendingAutoAction over the shared
// table the dashboard's pendingAutoAction runs over too (owner ruling R-D2):
// the job JSON decoded as a database.Job, as the wire carries it.
//
// Mutant: a case's want edited — this side or the node side fails.
func TestPendingAutoActionParity(t *testing.T) {
	p := loadJobStateParity(t)
	if len(p.PendingAutoAction) < 10 {
		t.Fatalf("parsed %d pendingAutoAction cases, want the table", len(p.PendingAutoAction))
	}
	for _, c := range p.PendingAutoAction {
		var job database.Job
		if err := json.Unmarshal(c.Job, &job); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got := jobstate.PendingAutoAction(&job); string(got) != c.Want {
			t.Errorf("%s: PendingAutoAction = %q, want %q", c.Name, got, c.Want)
		}
	}
}

// TestDashboardAutoRetryConstantsMatch pins utils.js's copy of the recovery
// constant its pendingAutoAction reads to the Go one — the retry budget (the
// park reasons themselves are pinned by the shared table; no error text is
// read on either side, owner ruling R-I1) — and the two action sentences to
// AutoAction.Text.
//
// Mutant: the constant or a sentence edited on one side only.
func TestDashboardAutoRetryConstantsMatch(t *testing.T) {
	src := readUtilsJS(t)
	for _, r := range []database.ParkReason{database.ParkReasonTwitchOffline, database.ParkReasonTwitchUnreachable, database.ParkReasonTwitchEndUnconfirmed} {
		if !strings.Contains(src, `job.parkReason === "`+string(r)+`"`) && !strings.Contains(src, `job.parkReason !== "`+string(r)+`"`) {
			t.Errorf("utils.js pendingAutoAction does not read park reason %q", r)
		}
	}
	if m := regexp.MustCompile(`(?m)^export const MAX_TWITCH_AUTO_RETRIES = (\d+);`).FindStringSubmatch(src); m == nil {
		t.Error("utils.js: MAX_TWITCH_AUTO_RETRIES not found")
	} else if n, _ := strconv.Atoi(m[1]); n != jobstate.MaxTwitchAutoRetries {
		t.Errorf("utils.js MAX_TWITCH_AUTO_RETRIES = %d, Go %d", n, jobstate.MaxTwitchAutoRetries)
	}
	for _, a := range []jobstate.AutoAction{jobstate.AutoActionMux, jobstate.AutoActionRetry} {
		want := `if (action === "` + string(a) + `") return "` + a.Text() + `";`
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(src) {
			t.Errorf("utils.js pendingAutoActionText does not say %q for %s", a.Text(), a)
		}
	}
}

func readUtilsJS(t *testing.T) string {
	t.Helper()
	const utilsFile = "../../web/public/modules/utils.js"
	src, err := os.ReadFile(utilsFile)
	if err != nil {
		t.Fatalf("read %s: %v", utilsFile, err)
	}
	return string(src)
}

// TestWaitLineParity runs jobstate.IsWaitLine over the lines the dashboard's
// isWaitLine runs over (owner ruling R-D1), and pins utils.js WAIT_MARKER to
// jobstate.WaitMarker — the TUI's task list, the dashboard's Upcoming rows
// and every Go writer of a wait line read one marker.
//
// Mutants: WAIT_MARKER edited in utils.js; a case's want edited.
func TestWaitLineParity(t *testing.T) {
	p := loadJobStateParity(t)
	if len(p.WaitLines) < 8 {
		t.Fatalf("parsed %d waitLines cases, want the table", len(p.WaitLines))
	}
	for _, c := range p.WaitLines {
		if got := jobstate.IsWaitLine(c.Line); got != c.Want {
			t.Errorf("IsWaitLine(%q) = %v, want %v", c.Line, got, c.Want)
		}
	}
	m := regexp.MustCompile(`(?m)^export const WAIT_MARKER = ("[^"]*");`).FindStringSubmatch(readUtilsJS(t))
	if m == nil {
		t.Fatal("utils.js: WAIT_MARKER not found")
	}
	if s, _ := strconv.Unquote(m[1]); s != jobstate.WaitMarker {
		t.Errorf("utils.js WAIT_MARKER = %q, jobstate.WaitMarker %q", s, jobstate.WaitMarker)
	}
}

// TestQueuedHoldParity runs jobstate.AdmissionHolds.For over the table the
// dashboard's queuedHold runs over (owner ruling R-D3), and pins utils.js
// HOLD_PRECEDENCE to jobstate.HoldPrecedence: one precedence, written once in
// each language and compared here.
//
// Mutants: HOLD_PRECEDENCE reordered in utils.js; a case's want edited.
func TestQueuedHoldParity(t *testing.T) {
	p := loadJobStateParity(t)
	if len(p.QueuedHolds) < 10 {
		t.Fatalf("parsed %d queuedHolds cases, want the table", len(p.QueuedHolds))
	}
	for _, c := range p.QueuedHolds {
		got := c.Holds.For(&c.Job, c.Now)
		w := c.Want
		if string(got.Reason) != w.Reason || !got.Until.Equal(w.Until) || got.Attempt != w.Attempt || got.Of != w.Of {
			t.Errorf("%s: For = %+v, want %+v", c.Name, got, w)
		}
	}
	m := regexp.MustCompile(`(?m)^export const HOLD_PRECEDENCE = \[([^\]]*)\];`).FindStringSubmatch(readUtilsJS(t))
	if m == nil {
		t.Fatal("utils.js: HOLD_PRECEDENCE not found")
	}
	var js []string
	for _, lit := range regexp.MustCompile(`"[^"]*"`).FindAllString(m[1], -1) {
		u, _ := strconv.Unquote(lit)
		js = append(js, u)
	}
	var goList []string
	for _, r := range jobstate.HoldPrecedence {
		goList = append(goList, string(r))
	}
	if strings.Join(js, ",") != strings.Join(goList, ",") {
		t.Errorf("utils.js HOLD_PRECEDENCE = %v, jobstate.HoldPrecedence = %v", js, goList)
	}
}

// TestCookieBadgePrecedenceMatchesTheDashboard pins owner ruling R-D4: the
// dashboard's COOKIE_BADGE_PRECEDENCE (utils.js, walked by
// cookieIndicatorState) is the TUI's tui.CookieBadgePrecedence, state for
// state and in order. They ranked an unreadable cookies.txt and a parked job
// in opposite orders, so one state read "YT?" in the TUI and red "a download
// stopped" on the dashboard.
//
// Mutant: either list reordered or a state renamed.
func TestCookieBadgePrecedenceMatchesTheDashboard(t *testing.T) {
	m := regexp.MustCompile(`(?m)^export const COOKIE_BADGE_PRECEDENCE = \[([^\]]*)\];`).FindStringSubmatch(readUtilsJS(t))
	if m == nil {
		t.Fatal("utils.js: COOKIE_BADGE_PRECEDENCE not found")
	}
	var js []string
	for _, lit := range regexp.MustCompile(`"[^"]*"`).FindAllString(m[1], -1) {
		u, _ := strconv.Unquote(lit)
		js = append(js, u)
	}
	if strings.Join(js, ",") != strings.Join(tui.CookieBadgePrecedence, ",") {
		t.Errorf("utils.js COOKIE_BADGE_PRECEDENCE = %v, tui.CookieBadgePrecedence = %v", js, tui.CookieBadgePrecedence)
	}
}

// TestAwaitingProcessingParity runs jobstate.AwaitingProcessing over the
// rows the dashboard's isAwaitingProcessing runs over (owner ruling R-D1),
// and pins the text the dashboard's formatProgress shows for such a row to
// jobstate.AwaitingProcessingText, the TUI details pane's line.
//
// Mutants: a case's want edited; the text edited in app.js only.
func TestAwaitingProcessingParity(t *testing.T) {
	p := loadJobStateParity(t)
	if len(p.AwaitingProcessing) < 6 {
		t.Fatalf("parsed %d awaitingProcessing cases, want the table", len(p.AwaitingProcessing))
	}
	for _, c := range p.AwaitingProcessing {
		var job database.Job
		if err := json.Unmarshal(c.Job, &job); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got := jobstate.AwaitingProcessing(&job); got != c.Want {
			t.Errorf("%s: AwaitingProcessing = %v, want %v", c.Name, got, c.Want)
		}
	}
	if jobstate.AwaitingProcessing(nil) {
		t.Error("AwaitingProcessing(nil) = true")
	}
	const appFile = "../../web/public/app.js"
	src, err := os.ReadFile(appFile)
	if err != nil {
		t.Fatalf("read %s: %v", appFile, err)
	}
	want := `if (isAwaitingProcessing(job)) return "` + jobstate.AwaitingProcessingText + `";`
	if !strings.Contains(string(src), want) {
		t.Errorf("app.js formatProgress does not say %q for an awaiting-processing row", jobstate.AwaitingProcessingText)
	}
}

// TestCookieRetryParity runs jobstate.AdmissionHolds.CookieRetryFor over the
// table the dashboard's cookieRetry runs over too (owner ruling R2-C1): which
// parked rows show their automatic cookie retries, and the 24-hour window to
// the second. The two sentences are compared as each side words them.
//
// Mutant: a case's want edited, or the window changed on one side only — this
// side or the node side fails.
func TestCookieRetryParity(t *testing.T) {
	p := loadJobStateParity(t)
	if len(p.CookieRetries) < 8 {
		t.Fatalf("parsed %d cookieRetries cases, want the table", len(p.CookieRetries))
	}
	for _, c := range p.CookieRetries {
		got := c.Holds.CookieRetryFor(&c.Job, c.Now)
		if got.Count != c.Want.Count || got.Of != c.Want.Of || !got.Last.Equal(c.Want.Last) {
			t.Errorf("%s: CookieRetryFor = %+v, want %+v", c.Name, got, c.Want)
		}
	}
	src := readUtilsJS(t)
	for _, r := range []jobstate.CookieRetry{{Count: 2, Of: 3}, {Count: 3, Of: 3}} {
		js := strings.NewReplacer("${retry.count}", strconv.Itoa(r.Count), "${retry.of}", strconv.Itoa(r.Of))
		found := false
		for _, m := range regexp.MustCompile("return `(Cookie retr[^`]*)`;").FindAllStringSubmatch(src, -1) {
			if js.Replace(m[1]) == r.Text() {
				found = true
			}
		}
		if !found {
			t.Errorf("utils.js cookieRetryText has no sentence that reads %q", r.Text())
		}
	}
}

// TestSuggestedChatOffsetParity runs jobstate.SuggestedChatOffset and
// LatencyLine over the table the dashboard's suggestedChatOffset and
// latencyText run over (owner ruling RV-19), and pins utils.js's six offsets
// and three mode labels to the Go ones.
//
// Mutants: a case's want edited; an offset or a label edited on one side.
func TestSuggestedChatOffsetParity(t *testing.T) {
	p := loadJobStateParity(t)
	if len(p.SuggestedChatOffset) < 10 {
		t.Fatalf("parsed %d suggestedChatOffset cases, want the table", len(p.SuggestedChatOffset))
	}
	for _, c := range p.SuggestedChatOffset {
		var job database.Job
		if err := json.Unmarshal(c.Job, &job); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		s, known, basis := jobstate.SuggestedChatOffset(&job)
		if s != c.Want.Seconds || known != c.Want.Known || string(basis) != c.Want.Basis {
			t.Errorf("%s: SuggestedChatOffset = %d %v %q, want %d %v %q", c.Name, s, known, basis, c.Want.Seconds, c.Want.Known, c.Want.Basis)
		}
		if got := jobstate.LatencyLine(&job); got != c.Want.Line {
			t.Errorf("%s: LatencyLine = %q, want %q", c.Name, got, c.Want.Line)
		}
	}

	src := readUtilsJS(t)
	for name, v := range map[string]int{
		"SUGGESTED_CHAT_OFFSET_INGEST_NORMAL":    jobstate.SuggestedChatOffsetIngestNormal,
		"SUGGESTED_CHAT_OFFSET_INGEST_LOW":       jobstate.SuggestedChatOffsetIngestLow,
		"SUGGESTED_CHAT_OFFSET_INGEST_ULTRA_LOW": jobstate.SuggestedChatOffsetIngestUltraLow,
		"SUGGESTED_CHAT_OFFSET_START_NORMAL":     jobstate.SuggestedChatOffsetStartNormal,
		"SUGGESTED_CHAT_OFFSET_START_LOW":        jobstate.SuggestedChatOffsetStartLow,
		"SUGGESTED_CHAT_OFFSET_START_ULTRA_LOW":  jobstate.SuggestedChatOffsetStartUltraLow,
	} {
		m := regexp.MustCompile(`(?m)^export const ` + name + ` = (\d+);`).FindStringSubmatch(src)
		if m == nil {
			t.Errorf("utils.js: %s not found", name)
			continue
		}
		if n, _ := strconv.Atoi(m[1]); n != v {
			t.Errorf("utils.js %s = %d, Go %d", name, n, v)
		}
	}
	for _, m := range []database.LatencyMode{database.LatencyModeNormal, database.LatencyModeLow, database.LatencyModeUltraLow} {
		if want := `  ` + string(m) + `: "` + m.Label() + `",`; !strings.Contains(src, want) {
			t.Errorf("utils.js LATENCY_MODE_LABELS does not hold %q", want)
		}
	}
}

// TestOperatorChatOffsetParity runs database.Job.HasOperatorChatOffset over
// the table the dashboard's hasOperatorChatOffset runs over: an operator's
// saved offset (a 0 included) always wins over the suggestion.
//
// Mutant: a case's want edited.
func TestOperatorChatOffsetParity(t *testing.T) {
	p := loadJobStateParity(t)
	if len(p.OperatorChatOffset) < 5 {
		t.Fatalf("parsed %d operatorChatOffset cases, want the table", len(p.OperatorChatOffset))
	}
	for _, c := range p.OperatorChatOffset {
		var job database.Job
		if err := json.Unmarshal(c.Job, &job); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got := job.HasOperatorChatOffset(); got != c.Want {
			t.Errorf("%s: HasOperatorChatOffset = %v, want %v", c.Name, got, c.Want)
		}
	}
}
