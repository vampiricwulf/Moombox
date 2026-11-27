package worker

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// framedToken is a stand-in for a minted PO token (never a real one): one
// field-6 record whose payload is itBytes of integrity token, the binding and
// the 12 opaque bytes a mint adds, so bgutils.ITBytes reads itBytes back only
// under the binding it was "minted" under.
func framedToken(itBytes int, binding string) string {
	payload := make([]byte, itBytes+len(binding)+12)
	for i := range payload {
		payload[i] = byte(i*7 + 3)
	}
	raw := binary.AppendUvarint([]byte{0x32}, uint64(len(payload)))
	return base64.URLEncoding.EncodeToString(append(raw, payload...))
}

// The "[POT]" lines that report a minted GVS token carry itBytes (owner
// ruling RV-18), read under the binding the token was minted under — a
// visitor-data binding here, far longer than the video ID, so reading it
// under the video ID or the binding's kind gives another number — and
// "unknown" for a token that cannot be read. Never the token itself.
//
// Mutants (each run): reading the first mint's line under the job's video ID
// fails the first subtest; dropping itBytes from that line fails both
// subtests.
func TestGvsMintLineReportsITBytes(t *testing.T) {
	const visitor = "CgtWaXNpdG9yRGF0YSiAgICAgICAgAEyCgoCVVMSBBoAIDQ%3D"
	for _, tc := range []struct {
		name string
		tok  string
		want any
	}{
		{"a minted token", framedToken(67, visitor), 67},
		{"an unreadable token", "tok123", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBinding string
			orig := mintGvsPoToken
			mintGvsPoToken = func(_ context.Context, _ *bgutils.PotProvider, binding string) (string, error) {
				gotBinding = binding
				return tc.tok, nil
			}
			t.Cleanup(func() { mintGvsPoToken = orig })
			job, logs := vodPotJob(t)
			info := vodPotInfo("web_creator", "web_creator")
			info.GvsBinding, info.GvsBindingKind = visitor, youtube.BindingVisitorData
			runVodPot(t, job, info, &bgutils.PotProvider{})
			if gotBinding != visitor {
				t.Fatalf("minted under %q, want the visitor data", gotBinding)
			}
			line := onlyPotLine(logs, "[POT] GVS mint")
			if line == nil || line["itBytes"] != tc.want {
				t.Errorf("[POT] GVS mint line = %v, want itBytes=%v", line, tc.want)
			}
		})
	}
}

// The 403 credential refresh's summary line carries the re-mint's itBytes,
// read under the binding it re-minted under, and no itBytes when no token was
// minted.
//
// Mutants (each run): dropping the summary's itBytes fails the first
// subtest; adding it whatever the mint did fails the second.
func TestCredentialRefreshLineReportsITBytes(t *testing.T) {
	const binding = "Cgt0aGUtc2V0dXAtYmluZGluZw%3D%3D"
	for _, tc := range []struct {
		name    string
		mintErr error
		want    any
	}{
		{"re-minted", nil, 68},
		{"the re-mint failed", errors.New("sidecar down"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePotProvider{generate: func(_ context.Context, b string, _ bool) (string, error) {
				if tc.mintErr != nil {
					return "", tc.mintErr
				}
				return framedToken(68, b), nil
			}}
			logs := &captureLogger{}
			job := &JobContext{Job: &database.Job{ID: "test-job"}, Logger: logs}
			info := &youtube.VideoInfo{Formats: []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1", Source: "web_creator"}}}
			refreshGvsCredentials(context.Background(), job, info, 140, nil, nil, fake, binding, "test")
			line := onlyPotLine(logs, "[POT] credential refresh")
			if line == nil {
				t.Fatalf("no [POT] credential refresh line: %v", logs.lines())
			}
			if line["itBytes"] != tc.want {
				t.Errorf("[POT] credential refresh line = %v, want itBytes=%v", line, tc.want)
			}
		})
	}
}

// A whole-file refresh whose re-extraction moved to another visitor session
// re-mints under the fresh binding; both its re-mint line and its summary
// read itBytes under that binding, not the setup's.
//
// Mutants (each run): reading either line under the setup binding fails it.
func TestVodURLRefreshLinesReportITBytesUnderTheFreshBinding(t *testing.T) {
	const player = "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"
	const old = "http://cdn.invalid/videoplayback?expire=1&id=o-A&itag=137&clen=1000"
	const setupBinding, freshBinding = "vd-A", "CgtmcmVzaC12aXNpdG9yLXNlc3Npb24tZGF0YQ%3D%3D"
	stubRefreshVideoInfo(t, func() *youtube.VideoInfo {
		return &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PlayerURL: player,
			GvsBinding: freshBinding, GvsBindingKind: youtube.BindingVisitorData,
			Formats: []youtube.Format{{Itag: 137, URL: "http://cdn.invalid/videoplayback?expire=2&id=o-B&itag=137&clen=1000", ContentLength: "1000", Source: "web_creator"}}}
	})
	minter := &fakePotProvider{generate: func(_ context.Context, b string, _ bool) (string, error) {
		return framedToken(66, b), nil
	}}
	job, logs := vodPotJob(t)
	job.YT = youtube.NewService(nil, &discardLogger{})
	st := newVodRefreshState(nil, youtube.Format{Itag: 137, URL: old, ContentLength: "1000", Source: "web_creator"}, old)
	_, tok := refreshVodURL(context.Background(), job, player, st, stubCipherSolver{}, nil, minter, setupBinding, "VOD video")
	if tok != framedToken(66, freshBinding) {
		t.Fatalf("refreshVodURL handed out another token than the fresh session's")
	}
	for _, msg := range []string{
		"[POT] VOD URL refresh: the re-extraction's visitor session moved — re-minted the token for it",
		"[POT] VOD URL refresh",
	} {
		if line := onlyPotLine(logs, msg); line == nil || line["itBytes"] != 66 {
			t.Errorf("%q line = %v, want itBytes=66", msg, line)
		}
	}
}

// onlyPotLine is the one line logged with exactly msg, or nil when there is
// not exactly one.
func onlyPotLine(logs *captureLogger, msg string) map[string]any {
	if lines := potLines(logs, msg); len(lines) == 1 {
		return lines[0]
	}
	return nil
}
