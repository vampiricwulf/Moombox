package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
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

// TestDashboardAutoRetryConstantsMatch pins utils.js's copies of the two
// recovery constants its pendingAutoAction reads to the Go ones, and the two
// action sentences to AutoAction.Text.
//
// Mutant: either constant or sentence edited on one side only.
func TestDashboardAutoRetryConstantsMatch(t *testing.T) {
	src := readUtilsJS(t)
	if m := regexp.MustCompile(`(?m)^export const TWITCH_OFFLINE_ERR_MSG = ("[^"]*");`).FindStringSubmatch(src); m == nil {
		t.Error("utils.js: TWITCH_OFFLINE_ERR_MSG not found")
	} else if s, _ := strconv.Unquote(m[1]); s != jobstate.TwitchOfflineErrMsg {
		t.Errorf("utils.js TWITCH_OFFLINE_ERR_MSG = %q, Go %q", s, jobstate.TwitchOfflineErrMsg)
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
