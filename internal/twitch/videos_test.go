package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// videoEdge renders one FilterableVideoTower_Videos edge, the shape yt-dlp's
// TwitchPlaylistBaseIE._entries reads (twitch.py b77c9304e): a VideoEdge with
// a cursor around a Video node. broadcastType is left out when bt is "".
func videoEdge(id, published, bt string) string {
	btField := ""
	if bt != "" {
		btField = fmt.Sprintf(`,"broadcastType":%q`, bt)
	}
	return fmt.Sprintf(`{"__typename":"VideoEdge","cursor":"c%s","node":{"__typename":"Video","id":%q,"title":"t%s","publishedAt":%q,"lengthSeconds":60%s}}`,
		id, id, id, published, btField)
}

func videosPage(userID string, edges ...string) string {
	return fmt.Sprintf(`{"data":{"user":{"id":%q,"videos":{"edges":[%s]}}}}`, userID, strings.Join(edges, ","))
}

// sentVariables decodes the variables of one persisted query (a single object)
// or the first of a batch.
func sentVariables(t *testing.T, body string) (string, map[string]any) {
	t.Helper()
	var q struct {
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if strings.HasPrefix(body, "[") {
		var qs []json.RawMessage
		if err := json.Unmarshal([]byte(body), &qs); err != nil || len(qs) == 0 {
			t.Fatalf("bad batch %s", body)
		}
		body = string(qs[0])
	}
	if err := json.Unmarshal([]byte(body), &q); err != nil {
		t.Fatalf("bad request %s: %v", body, err)
	}
	return q.OperationName, q.Variables
}

// TestEachChannelVideoPagesByCursor is TwitchPlaylistBaseIE._entries (twitch.py
// b77c9304e): limit 100, broadcastType null (the server-side filter returns 0
// entries — twitch.py 735-761), the last kept edge's cursor sent for the next
// page, non-VideoEdge edges and non-Video nodes skipped, and the walk ending
// on a page without edges.
//
// Mutants: the cursor not sent (page 1 again); broadcastType sent as the
// filter's type; the edge typename check dropped (the ClipEdge's node kept);
// the node typename check dropped (the Clip node kept).
func TestEachChannelVideoPagesByCursor(t *testing.T) {
	var sent []map[string]any
	stubGQL(t, func(body string) string {
		op, vars := sentVariables(t, body)
		if op != "FilterableVideoTower_Videos" || !strings.Contains(body, constants.TwitchGQLHashes.FilterableVideoTowerVideos) {
			t.Fatalf("op %s, want FilterableVideoTower_Videos", op)
		}
		sent = append(sent, vars)
		if len(sent) > 5 {
			t.Errorf("page %d requested: the walk is not advancing its cursor", len(sent))
			return videosPage("1")
		}
		switch vars["cursor"] {
		case nil:
			return videosPage("1", videoEdge("3", "2026-10-03T00:00:00Z", ""),
				`{"__typename":"ClipEdge","cursor":"bad","node":{"__typename":"Video","id":"x"}}`,
				`{"__typename":"VideoEdge","cursor":"bad","node":{"__typename":"Clip","id":"y"}}`,
				videoEdge("2", "2026-10-02T00:00:00Z", ""))
		case "c2":
			return videosPage("1", videoEdge("1", "2026-10-01T00:00:00Z", ""))
		case "c1":
			return videosPage("1")
		}
		t.Fatalf("unexpected cursor %v", vars["cursor"])
		return ""
	})
	var ids []string
	err := NewAPI(&testLogger{}).EachChannelVideo(context.Background(), "Streamer", "time", 0, "", func(v TwitchVideo) bool {
		ids = append(ids, v.ID)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "3,2,1" {
		t.Errorf("ids = %v, want 3,2,1", ids)
	}
	if len(sent) != 3 {
		t.Fatalf("%d pages requested, want 3", len(sent))
	}
	for _, v := range sent {
		if v["channelOwnerLogin"] != "streamer" || v["videoSort"] != "TIME" || v["limit"] != float64(100) {
			t.Errorf("variables %v, want the lower-cased login, TIME, limit 100", v)
		}
		if bt, ok := v["broadcastType"]; !ok || bt != nil {
			t.Errorf("broadcastType = %v (present %v), want null", bt, ok)
		}
	}
}

// TestEachChannelVideoUnknownLogin is twitch.py 672-675 (yt-dlp #15450): a
// user whose id is "" is a login that does not resolve, and the videos beside
// it are not that channel's.
//
// Mutant: the guard removed — the stranger's videos are returned.
func TestEachChannelVideoUnknownLogin(t *testing.T) {
	stubGQL(t, func(body string) string {
		if _, vars := sentVariables(t, body); vars["cursor"] != nil {
			return videosPage("")
		}
		return videosPage("", videoEdge("9", "2026-10-03T00:00:00Z", "ARCHIVE"))
	})
	n := 0
	err := NewAPI(&testLogger{}).EachChannelVideo(context.Background(), "nobody", "time", 0, "", func(TwitchVideo) bool { n++; return true })
	if !errors.Is(err, ErrChannelNotFound) || n != 0 {
		t.Errorf("err = %v, visited %d; want ErrChannelNotFound and none", err, n)
	}
}

// TestListChannelVideosFiltersClientSide: a /<ch>/videos?filter=archives
// listing keeps the ARCHIVE videos, read off the node when it says and from
// VideoMetadata when it does not, and asks Twitch for every type.
//
// Mutants: the node's own broadcastType ignored (a VideoMetadata request per
// node — the stub fails the test on one for "h"); VideoMetadata not consulted
// for a node without one (the unlabelled archive "a2" dropped).
func TestListChannelVideosFiltersClientSide(t *testing.T) {
	stubGQL(t, func(body string) string {
		op, vars := sentVariables(t, body)
		switch op {
		case "FilterableVideoTower_Videos":
			if vars["cursor"] != nil {
				return videosPage("1")
			}
			return videosPage("1",
				videoEdge("a1", "2026-10-03T00:00:00Z", "ARCHIVE"),
				videoEdge("h", "2026-10-02T00:00:00Z", "HIGHLIGHT"),
				videoEdge("a2", "2026-10-01T00:00:00Z", ""))
		case "VideoMetadata":
			if vars["videoID"] != "a2" {
				t.Errorf("VideoMetadata asked for %v; only the unlabelled node needs it", vars["videoID"])
			}
			return `{"data":{"video":{"id":"a2","title":"x","broadcastType":"ARCHIVE","owner":{"login":"streamer"}}}}`
		}
		t.Fatalf("unexpected op %s", op)
		return ""
	})
	got, err := NewAPI(&testLogger{}).ListChannelVideos(context.Background(), "streamer", VideoListBroadcastType("archives"), "time", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, v := range got {
		ids = append(ids, v.ID)
	}
	if strings.Join(ids, ",") != "a1,a2" {
		t.Errorf("kept %v, want a1,a2", ids)
	}
}

// TestVideoListFilterMapping is TwitchVideosIE._BROADCASTS and _SORTED_BY.
//
// Mutant: an unknown filter mapped to ARCHIVE instead of all.
func TestVideoListFilterMapping(t *testing.T) {
	for in, want := range map[string]string{"archives": "ARCHIVE", "highlights": "HIGHLIGHT", "uploads": "UPLOAD",
		"past_premieres": "PAST_PREMIERE", "all": "", "": "", "bogus": ""} {
		if got := VideoListBroadcastType(in); got != want {
			t.Errorf("VideoListBroadcastType(%q) = %q, want %q", in, got, want)
		}
	}
	if VideoListSort("views") != "VIEWS" || VideoListSort("") != "TIME" || VideoListSort("time") != "TIME" {
		t.Error("VideoListSort: views is VIEWS, everything else TIME")
	}
}

// TestCollectionVideos is TwitchCollectionIE: one CollectionSideBar request
// with the collection ID, its title and every node with an id; a null
// collection is not found.
//
// Mutants: a node without an id kept; a null collection read as empty.
func TestCollectionVideos(t *testing.T) {
	stubGQL(t, func(body string) string {
		op, vars := sentVariables(t, body)
		if op != "CollectionSideBar" || vars["collectionID"] != "o9zZer3IQBhTJw" ||
			!strings.Contains(body, constants.TwitchGQLHashes.CollectionSideBar) {
			t.Fatalf("request %s", body)
		}
		return `{"data":{"collection":{"title":"Playthrough Archives","items":{"edges":[{"node":{"id":"1","title":"one","owner":{"login":"a"}}},{"node":{"title":"no id"}},{"node":{"id":"2","title":"two","owner":{"login":"a"}}}]}}}}`
	})
	title, vids, err := NewAPI(&testLogger{}).CollectionVideos(context.Background(), "o9zZer3IQBhTJw", "")
	if err != nil || title != "Playthrough Archives" || len(vids) != 2 || vids[0].ID != "1" || vids[1].ID != "2" {
		t.Fatalf("CollectionVideos = %q, %+v, %v", title, vids, err)
	}

	stubGQL(t, func(string) string { return `{"data":{"collection":null}}` })
	if _, _, err := NewAPI(&testLogger{}).CollectionVideos(context.Background(), "gone", ""); !errors.Is(err, ErrCollectionNotFound) {
		t.Errorf("null collection: err = %v, want ErrCollectionNotFound", err)
	}
}

// TestArchiveVodBetween is yt-dlp's live_from_start lookup with the audit's
// corrections (owner decision D17): the list asked for with broadcastType
// null, the candidate confirmed ARCHIVE through VideoMetadata, published at or
// after the stream's start — and, with the broadcast's end known, before it,
// the OLDEST such archive taken.
//
// Mutants: the publishedAt >= start test dropped ("older" taken in the
// deleted-archive row); the end bound dropped ("next" taken there); the newest
// candidate tried first ("next" taken in the no-end row); the VideoMetadata
// confirmation skipped ("up" taken in the upload-and-highlight row).
func TestArchiveVodBetween(t *testing.T) {
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(4 * time.Hour)
	types := map[string]string{"next": "ARCHIVE", "hl": "HIGHLIGHT", "a": "ARCHIVE", "older": "ARCHIVE", "up": "UPLOAD"}
	cases := []struct {
		name  string
		edges []string
		end   time.Time
		want  string
	}{
		{"oldest archive in the broadcast", []string{
			videoEdge("next", "2026-10-02T12:00:05Z", ""),
			videoEdge("hl", "2026-10-01T14:00:00Z", ""),
			videoEdge("a", "2026-10-01T12:00:04Z", ""),
			videoEdge("older", "2026-09-30T12:00:00Z", ""),
		}, end, "a"},
		{"no end: the oldest archive after start", []string{
			videoEdge("next", "2026-10-02T12:00:05Z", ""),
			videoEdge("a", "2026-10-01T12:00:04Z", ""),
		}, time.Time{}, "a"},
		{"only an upload and a highlight in the broadcast", []string{
			videoEdge("hl", "2026-10-01T14:00:00Z", ""),
			videoEdge("up", "2026-10-01T13:00:00Z", ""),
			videoEdge("older", "2026-09-30T12:00:00Z", ""),
		}, end, ""},
		{"archive deleted, the next broadcast's remains", []string{
			videoEdge("next", "2026-10-02T12:00:05Z", ""),
			videoEdge("older", "2026-09-30T12:00:00Z", ""),
		}, end, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubGQL(t, func(body string) string {
				op, vars := sentVariables(t, body)
				switch op {
				case "FilterableVideoTower_Videos":
					if bt, ok := vars["broadcastType"]; !ok || bt != nil {
						t.Errorf("broadcastType = %v, want null", bt)
					}
					if vars["cursor"] != nil {
						return videosPage("1")
					}
					return videosPage("1", c.edges...)
				case "VideoMetadata":
					id, _ := vars["videoID"].(string)
					return fmt.Sprintf(`{"data":{"video":{"id":%q,"title":"x","broadcastType":%q,"owner":{"login":"streamer"}}}}`, id, types[id])
				}
				t.Fatalf("unexpected op %s", op)
				return ""
			})
			got, err := NewAPI(&testLogger{}).ArchiveVodBetween(context.Background(), "Streamer", start, c.end, "")
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("ArchiveVodBetween = %q, want %q", got, c.want)
			}
		})
	}
}

// TestListChannelVideosKeepsWhatWasReadOnALaterPageError is yt-dlp's
// _entries on a failed page (`fatal=False`, then `if not page: break` —
// twitch.py b77c9304e): a page after the first that fails ends the listing
// with the videos already read; a first page that fails is the error.
//
// Mutant: any page error failing the listing (the 50-page catalogue that
// hits one error on page 2 queues nothing).
func TestListChannelVideosKeepsWhatWasReadOnALaterPageError(t *testing.T) {
	stubGQL(t, func(body string) string {
		_, vars := sentVariables(t, body)
		if vars["cursor"] == nil {
			return videosPage("1", videoEdge("2", "2026-10-02T00:00:00Z", ""), videoEdge("1", "2026-10-01T00:00:00Z", ""))
		}
		return `not json`
	})
	vids, err := NewAPI(&testLogger{}).ListChannelVideos(context.Background(), "streamer", "", "time", 0, "")
	if err != nil || len(vids) != 2 || vids[0].ID != "2" || vids[1].ID != "1" {
		t.Errorf("ListChannelVideos = %+v, %v; want page 1's two videos and no error", vids, err)
	}

	stubGQL(t, func(string) string { return `not json` })
	if vids, err := NewAPI(&testLogger{}).ListChannelVideos(context.Background(), "streamer", "", "time", 0, ""); err == nil {
		t.Errorf("a failed first page = %+v, nil; want the error", vids)
	}
}

// TestCollectionVideosResolvesMissingOwners: a collection node that names no
// owner — yt-dlp reads none off it — takes VideoMetadata's, so its backlog
// row has a channel to be paced under; a node that names one is not asked
// about.
//
// Mutant: the owner lookup removed (the owner-less node stays without one).
func TestCollectionVideosResolvesMissingOwners(t *testing.T) {
	stubGQL(t, func(body string) string {
		op, vars := sentVariables(t, body)
		switch op {
		case "CollectionSideBar":
			return `{"data":{"collection":{"title":"Mixed","items":{"edges":[{"node":{"id":"1","title":"one","owner":{"login":"alice","displayName":"Alice"}}},{"node":{"id":"2","title":"two"}}]}}}}`
		case "VideoMetadata":
			if vars["videoID"] != "2" {
				t.Errorf("VideoMetadata asked for %v; only the owner-less node needs it", vars["videoID"])
			}
			return `{"data":{"video":{"id":"2","title":"two","owner":{"id":"9","login":"bob","displayName":"Bob"}}}}`
		}
		t.Fatalf("unexpected op %s", op)
		return ""
	})
	_, vids, err := NewAPI(&testLogger{}).CollectionVideos(context.Background(), "c", "")
	if err != nil || len(vids) != 2 {
		t.Fatalf("CollectionVideos = %+v, %v", vids, err)
	}
	if vids[0].Owner == nil || vids[0].Owner.Login != "alice" {
		t.Errorf("first node's owner %+v, want its own", vids[0].Owner)
	}
	if vids[1].Owner == nil || vids[1].Owner.Login != "bob" || vids[1].Owner.DisplayName != "Bob" {
		t.Errorf("owner-less node's owner %+v, want VideoMetadata's bob", vids[1].Owner)
	}
}
