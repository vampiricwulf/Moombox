package twitch

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestLivePlaybackTokenShape is Arc 10 Task 0: the ONE live measurement that
// decides whether the HLS half of this arc is buildable.
//
// The question. GetStreamAccessToken returns TwitchAccessToken{Value,
// Signature} and nothing else, and both halves are used opaquely as Usher
// query parameters — so today a DEAD auth-token produces a perfectly ordinary
// success: an ANONYMOUS playback token, served stitched ads and refused
// subscriber-only content, with nothing above Info in the log to say so. The
// token's Value is a JSON document. If that document states which session it
// was issued to, GetHLSMasterPlaylist can mark the platform the same way the
// chat handshake does, and a job with chat capture OFF stops being blind. If
// it does not, the finding is recorded and nothing is built on that side.
//
// WHAT THIS PRINTS, and why the rule is absolute. Field NAMES, JSON TYPES,
// BOOLEAN values, and the set difference between the two replies. Never a
// string value, never a number, never the Signature. The document is a signed
// entitlement: it carries a device id, a user ip and the token Usher accepts,
// and this file's output goes to a terminal, a CI log, and into a report an
// operator may paste. There is no field here worth reading whose value is
// worth printing — the whole question is answered by names, types and bools.
//
// Enable with:
//
//	MOOMBOX_LIVE_TWITCH_COOKIES=<path to a Netscape cookie file for a
//	                             signed-in Twitch session>
//	MOOMBOX_LIVE_TWITCH_CHANNEL=<the login of a channel that is LIVE right now>
//
// Always run with -count=1: a cached PASS on a live probe is not a fresh
// measurement, and the report this test feeds is only as current as its
// last real round trip.
//
// The path alone is the credential opt-in, matching TestLiveAuthenticatedToken-
// Validate in auth_live_test.go. The channel is a second required input rather
// than a hardcoded default: a default would rot, and an offline channel yields
// no stream playback token at all.
func TestLivePlaybackTokenShape(t *testing.T) {
	path := os.Getenv("MOOMBOX_LIVE_TWITCH_COOKIES")
	if path == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_COOKIES=<path to a signed-in Netscape cookie file> to run the playback-token shape probe")
	}
	channel := os.Getenv("MOOMBOX_LIVE_TWITCH_CHANNEL")
	if channel == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_CHANNEL=<login of a channel that is live right now> to run the playback-token shape probe")
	}

	jar := cookies.NewCookieJar()
	// Load reports only the path on failure, never file contents.
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	auth := NewAuth(jar, nopLogger{})
	if !auth.HasAuthToken() {
		t.Fatal("that cookie file carries no Twitch auth-token cookie — check the export, or that the path is the right file")
	}

	api := NewAPI(nopLogger{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The authenticated reply. auth.GetAuthToken() is read once and passed
	// straight through; it is never assigned to a variable this test prints.
	authed, err := api.GetStreamAccessToken(ctx, channel, auth.GetAuthToken())
	if err != nil {
		t.Fatalf("authenticated GetStreamAccessToken failed: %.300s", err)
	}
	// The anonymous reply. An empty token makes doGQLOnce omit the
	// Authorization header entirely (api.go), which is the same request an
	// install with no Twitch cookies makes.
	anon, err := api.GetStreamAccessToken(ctx, channel, "")
	if err != nil {
		t.Fatalf("anonymous GetStreamAccessToken failed: %.300s", err)
	}

	authedShape := describePlaybackToken(t, "authenticated", authed.Value)
	anonShape := describePlaybackToken(t, "anonymous", anon.Value)

	// The set difference is the answer. A key present in one reply and not the
	// other, or a boolean that differs, is a statement about the session; two
	// identical shapes mean the reply cannot tell us and branch B applies.
	reportKeyDifference(t, authedShape, anonShape)
}

// playbackTokenField is one top-level key of the playback token document,
// reduced to the three things that may be reported.
type playbackTokenField struct {
	name string
	kind string // "bool" | "number" | "string" | "null" | "object" | "array"
	// boolValue is meaningful only when kind == "bool". Every other kind
	// reports its TYPE and stops: see the file comment.
	boolValue bool
}

// describePlaybackToken decodes one token Value and logs its shape.
//
// The decode is of the RAW field. BuildUsherLiveURL percent-encodes Value when
// it builds the Usher URL (url.Values.Encode), so the struct field itself is
// plain JSON and needs no unescaping. If json.Unmarshal fails here, try
// url.QueryUnescape ONCE by hand, record that in the report, and do not add
// the unescape to production code on the strength of one observation.
func describePlaybackToken(t *testing.T, label, value string) map[string]playbackTokenField {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &doc); err != nil {
		// Only the error's TYPE is printed, never its message: encoding/json
		// error messages (SyntaxError, UnmarshalTypeError, ...) quote a
		// fragment of the offending input, which here would be a fragment of
		// the signed entitlement document.
		t.Fatalf("%s reply: the playback token Value is not a JSON object (error type %T) — "+
			"record this in the report and take branch B", label, err)
	}
	out := make(map[string]playbackTokenField, len(doc))
	names := make([]string, 0, len(doc))
	for name, raw := range doc {
		f := playbackTokenField{name: name, kind: jsonKindOf(raw)}
		if f.kind == "bool" {
			// The one value class that is safe to print, and the one most
			// likely to carry the answer.
			_ = json.Unmarshal(raw, &f.boolValue)
		}
		out[name] = f
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := out[name]
		if f.kind == "bool" {
			t.Logf("%s: %s = bool(%v)", label, f.name, f.boolValue)
			continue
		}
		t.Logf("%s: %s = %s", label, f.name, f.kind)
	}
	return out
}

// jsonKindOf names a raw JSON value's type without decoding it.
func jsonKindOf(raw json.RawMessage) string {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			return "object"
		case '[':
			return "array"
		case '"':
			return "string"
		case 't', 'f':
			return "bool"
		case 'n':
			return "null"
		default:
			return "number"
		}
	}
	return "null"
}

// reportKeyDifference logs which keys and which booleans separate the two
// replies. This is the finding Task 8 branches on.
func reportKeyDifference(t *testing.T, authed, anon map[string]playbackTokenField) {
	t.Helper()
	var onlyAuthed, onlyAnon, differingFields, sameShape []string
	for name, f := range authed {
		other, ok := anon[name]
		if !ok {
			onlyAuthed = append(onlyAuthed, name)
			continue
		}
		switch {
		case f.kind != other.kind:
			differingFields = append(differingFields,
				name+": authenticated="+f.kind+" anonymous="+other.kind)
		case f.kind == "bool" && f.boolValue != other.boolValue:
			differingFields = append(differingFields,
				name+": authenticated=bool("+boolText(f.boolValue)+") anonymous=bool("+boolText(other.boolValue)+")")
		default:
			sameShape = append(sameShape, name)
		}
	}
	for name := range anon {
		if _, ok := authed[name]; !ok {
			onlyAnon = append(onlyAnon, name)
		}
	}
	sort.Strings(onlyAuthed)
	sort.Strings(onlyAnon)
	sort.Strings(differingFields)
	sort.Strings(sameShape)

	t.Logf("KEYS ONLY IN THE AUTHENTICATED REPLY: %v", onlyAuthed)
	t.Logf("KEYS ONLY IN THE ANONYMOUS REPLY:     %v", onlyAnon)
	t.Logf("KEYS WHOSE TYPE OR BOOLEAN DIFFERS:   %v", differingFields)
	t.Logf("KEYS THAT LOOK THE SAME IN BOTH:      %v", sameShape)

	if len(onlyAuthed) == 0 && len(onlyAnon) == 0 && len(differingFields) == 0 {
		t.Log("FINDING: the two replies are indistinguishable by name, type and boolean. " +
			"Arc 10 Task 8 takes BRANCH B — record the finding in the spec docs and build nothing on the HLS side.")
		return
	}
	t.Log("FINDING: the reply distinguishes an authenticated session from an anonymous one. " +
		"Arc 10 Task 8 takes BRANCH A — the discriminating key above is what PlaybackTokenSession reads.")
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
