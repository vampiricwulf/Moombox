package youtube

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// ytText is an Innertube text object, which carries its string either as
// simpleText or as runs — the two shapes yt-dlp's _get_text reads.
type ytText struct {
	SimpleText string `json:"simpleText"`
	Runs       []struct {
		Text string `json:"text"`
	} `json:"runs"`
}

func (t *ytText) String() string {
	if t == nil {
		return ""
	}
	if t.SimpleText != "" {
		return t.SimpleText
	}
	var b strings.Builder
	for _, r := range t.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

// chapterInitialData decodes only the two chapter paths out of a watch page's
// ytInitialData.
type chapterInitialData struct {
	PlayerOverlays struct {
		PlayerOverlayRenderer struct {
			DecoratedPlayerBarRenderer struct {
				DecoratedPlayerBarRenderer struct {
					PlayerBar struct {
						ChapteredPlayerBarRenderer struct {
							Chapters []struct {
								ChapterRenderer *struct {
									TimeRangeStartMillis *float64 `json:"timeRangeStartMillis"`
									Title                *ytText  `json:"title"`
								} `json:"chapterRenderer"`
							} `json:"chapters"`
						} `json:"chapteredPlayerBarRenderer"`
					} `json:"playerBar"`
				} `json:"decoratedPlayerBarRenderer"`
			} `json:"decoratedPlayerBarRenderer"`
		} `json:"playerOverlayRenderer"`
	} `json:"playerOverlays"`
	EngagementPanels []struct {
		EngagementPanelSectionListRenderer struct {
			Content struct {
				MacroMarkersListRenderer *struct {
					Contents []struct {
						MacroMarkersListItemRenderer *struct {
							TimeDescription *ytText `json:"timeDescription"`
							Title           *ytText `json:"title"`
						} `json:"macroMarkersListItemRenderer"`
					} `json:"contents"`
				} `json:"macroMarkersListRenderer"`
			} `json:"content"`
		} `json:"engagementPanelSectionListRenderer"`
	} `json:"engagementPanels"`
}

// chaptersFromInitialData is yt-dlp's chapter order over a watch page's
// ytInitialData (_video.py b77c9304e, _real_extract → info['chapters']):
// the player bar's chapters (_extract_chapters_from_json: chapterRenderer
// timeRangeStartMillis and title), else the first engagement panel whose
// macro markers yield any (_extract_chapters_from_engagement_panel:
// timeDescription and title text). Both go through _extract_chapters_helper
// in its strict, unsorted mode (utils.ChaptersFromStarts). The description
// fallback is the caller's (FetchChapters).
func chaptersFromInitialData(d *chapterInitialData, durationMs int64) []utils.Chapter {
	var bar []utils.Chapter
	for _, c := range d.PlayerOverlays.PlayerOverlayRenderer.DecoratedPlayerBarRenderer.DecoratedPlayerBarRenderer.PlayerBar.ChapteredPlayerBarRenderer.Chapters {
		start := int64(-1)
		title := ""
		if c.ChapterRenderer != nil {
			if c.ChapterRenderer.TimeRangeStartMillis != nil {
				start = int64(*c.ChapterRenderer.TimeRangeStartMillis)
			}
			title = c.ChapterRenderer.Title.String()
		}
		bar = append(bar, utils.Chapter{StartMs: start, Title: title})
	}
	if out := utils.ChaptersFromStarts(bar, durationMs, false); len(out) > 0 {
		return out
	}
	for _, p := range d.EngagementPanels {
		markers := p.EngagementPanelSectionListRenderer.Content.MacroMarkersListRenderer
		if markers == nil {
			continue
		}
		var list []utils.Chapter
		for _, item := range markers.Contents {
			r := item.MacroMarkersListItemRenderer
			if r == nil {
				// traverse_obj's (..., 'macroMarkersListItemRenderer') skips
				// an entry without one.
				continue
			}
			list = append(list, utils.Chapter{StartMs: utils.ParseClockMs(r.TimeDescription.String()), Title: r.Title.String()})
		}
		if out := utils.ChaptersFromStarts(list, durationMs, false); len(out) > 0 {
			return out
		}
	}
	return nil
}

// ChaptersFromWatchPage reads a watch page's chapters out of its
// ytInitialData (chaptersFromInitialData); nil when the page carries none.
func ChaptersFromWatchPage(body []byte, durationMs int64) []utils.Chapter {
	var out []utils.Chapter
	extractYtInitialDataInto(body, func(obj []byte) bool {
		var d chapterInitialData
		if json.Unmarshal(obj, &d) != nil {
			return false
		}
		out = chaptersFromInitialData(&d, durationMs)
		return true
	})
	return out
}

// chapterPageTimeout bounds the one watch-page fetch FetchChapters makes. The
// chapters are a finalize-time extra: a slow page must not hold the mux.
const chapterPageTimeout = 20 * time.Second

// FetchChapters is a video's chapter list, in yt-dlp's order (owner decision
// D23 follow-up 2): the watch page's own chapter markers
// (ChaptersFromWatchPage), else the description's timestamps
// (utils.ChaptersFromDescription). Upstream reads the description only when
// the page had an initial data object at all; here a page that could not be
// fetched still leaves the description, which Moombox already has, so a
// network blip at finalize does not cost a timestamped description its
// chapters. Read at the mux, from the page as it is then, because a finished
// stream's chapters are typically added after it ends. Never an error: no
// chapters is a valid answer and the mux goes on either way.
func (s *Service) FetchChapters(ctx context.Context, videoID, description string, durationMs int64) []utils.Chapter {
	if durationMs <= 0 {
		return nil
	}
	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
	}
	if ck := s.GetCookieHeader(); ck != "" {
		headers["Cookie"] = ck
	}
	if body, err := fetchYouTubePage(ctx, "watch page", watchPageURL(videoID), chapterPageTimeout, headers, utils.MaxFetchBodySize); err == nil {
		if out := ChaptersFromWatchPage(body, durationMs); len(out) > 0 {
			return out
		}
	}
	return utils.ChaptersFromDescription(description, durationMs)
}
