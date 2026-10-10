package routes

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// ChannelRoutes registers channel-related API routes. The Store carries
// the cfg pointer + lock; SaveLocked persists to disk under the same lock
// so a rollback can restore the in-memory channel slice if the save fails.
// rl bounds POST /api/resolve-channel and the URL-resolving branch of POST
// /api/config/channels (each a youtube.com fetch with retries per call); nil
// leaves them unbounded.
func ChannelRoutes(r chi.Router, store *config.Store, onChannelChange func(), rl *web.RateLimiter) {
	mu := store.RWMutex()
	cfg := store.Config()

	// saveChannel validates and upserts one channel — POST
	// /api/config/channels once its ID is final. edit is the request's mark
	// that it means to replace a configured channel.
	saveChannel := func(rw http.ResponseWriter, channel config.ChannelConfig, edit bool) {
		// PUT /api/config's rule for the same field. The monitors treat any
		// platform that is not "twitch" as YouTube, so an unknown one was
		// accepted here, polled as a YouTube channel, and then made every
		// later full-form save 400 on a field the operator never touched.
		if !validChannelPlatform(channel.Platform) {
			jsonError(rw, "platform must be youtube or twitch", http.StatusBadRequest)
			return
		}
		// The overrides Save's Validate refuses — refused here by name
		// instead of failing the save into a bare 500.
		if fieldErrs := config.ChannelOverrideErrors(channel); fieldErrs != nil {
			msgs := slices.Sorted(maps.Values(fieldErrs))
			jsonError(rw, strings.Join(msgs, "; "), http.StatusBadRequest)
			return
		}

		// Upsert — copy-on-write: mutate a CLONE and assign the whole slice.
		// Store.Snapshot() readers (GET /api/config marshals after releasing
		// the lock) share the previous backing array, so writing an element
		// in place would race their reads; whole-slice replacement is the
		// documented Store contract. The old header doubles as the rollback
		// snapshot since its array is never touched. The ID matches
		// case-insensitively, the rule config.Validate refuses a duplicate
		// by: "Shroud" over a stored "shroud" is that channel, not a second
		// entry Save would then refuse.
		//
		// Replacing a configured channel takes the request's edit mark — the
		// dashboard's Edit dialog and its enable toggle send it. Unmarked,
		// the post is an add, and an add naming a configured channel is a
		// 409: the dashboard's Add Channel used to post {id, enabled} over
		// one, wiping its terms, output directory and overrides behind
		// "Channel added", and a client whose list is stale still could. A
		// marked edit of a channel removed meanwhile adds it back: the
		// operator saved it on purpose.
		mu.Lock()
		oldChannels := cfg.Channels
		newChannels := slices.Clone(cfg.Channels)
		found := false
		for i, ch := range newChannels {
			if strings.EqualFold(ch.ID, channel.ID) {
				if !edit {
					mu.Unlock()
					jsonError(rw, "channel "+ch.ID+" is already configured", http.StatusConflict)
					return
				}
				newChannels[i] = channel
				found = true
				break
			}
		}
		if !found {
			newChannels = append(newChannels, channel)
		}
		cfg.Channels = newChannels

		// Persist to disk; restore on save failure so in-memory and disk stay in sync.
		if err := store.SaveLocked(); err != nil {
			cfg.Channels = oldChannels
			mu.Unlock()
			jsonError(rw, "failed to save config", http.StatusInternalServerError)
			return
		}
		mu.Unlock()

		if onChannelChange != nil {
			onChannelChange()
		}

		jsonResponse(rw, map[string]any{"success": true, "channel": channel})
	}

	// POST /api/config/channels
	r.Post("/api/config/channels", func(rw http.ResponseWriter, req *http.Request) {
		// The channel, plus "edit": true when the request means to replace
		// a configured one (saveChannel). The mark is never stored.
		var body struct {
			config.ChannelConfig
			Edit bool `json:"edit"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid channel config", http.StatusBadRequest)
			return
		}
		channel := body.ChannelConfig

		if strings.TrimSpace(channel.ID) == "" {
			jsonError(rw, "channel ID required", http.StatusBadRequest)
			return
		}

		// The ID goes through utils.NormalizeChannelID, the normaliser
		// every channel writer shares: trimmed, and a URL or a bare
		// @handle resolved to the channel's ID. Resolving is a youtube.com
		// fetch with retries — the reason POST /api/resolve-channel is
		// rate limited — so it rides the same limiter here; a plain ID
		// (every enable/disable toggle posts one) does not. One that does
		// not resolve is refused rather than stored: the monitor would
		// poll channel_id=https://… forever.
		normalize := func(rw http.ResponseWriter, req *http.Request) {
			resolved, err := normalizeChannelID(req.Context(), channel.ID)
			if err != nil {
				msg, status := channelIDRefusal(err)
				jsonError(rw, msg, status)
				return
			}
			applyResolvedChannel(&channel, resolved)
			saveChannel(rw, channel, body.Edit)
		}
		if utils.NeedsChannelResolve(channel.ID) {
			limitedBy(rl)(http.HandlerFunc(normalize)).ServeHTTP(rw, req)
			return
		}
		normalize(rw, req)
	})

	// DELETE /api/config/channels/{id} is ChannelRemovalRoutes': removing
	// a channel asks what to do with its jobs.

	// PUT /api/config/channels/reorder
	r.Put("/api/config/channels/reorder", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		mu.Lock()

		if len(body.IDs) != len(cfg.Channels) {
			mu.Unlock()
			jsonError(rw, "ids count must match channels count", http.StatusBadRequest)
			return
		}

		// Reject duplicate IDs
		seen := make(map[string]bool, len(body.IDs))
		for _, id := range body.IDs {
			if seen[id] {
				mu.Unlock()
				jsonError(rw, "duplicate channel ID: "+id, http.StatusBadRequest)
				return
			}
			seen[id] = true
		}

		// Build lookup of existing channels by ID
		lookup := make(map[string]config.ChannelConfig, len(cfg.Channels))
		for _, ch := range cfg.Channels {
			lookup[ch.ID] = ch
		}

		// Reorder channels to match the provided ID order
		reordered := make([]config.ChannelConfig, 0, len(body.IDs))
		for _, id := range body.IDs {
			ch, ok := lookup[id]
			if !ok {
				mu.Unlock()
				jsonError(rw, "unknown channel ID: "+id, http.StatusBadRequest)
				return
			}
			reordered = append(reordered, ch)
		}

		oldChannels := cfg.Channels
		cfg.Channels = reordered

		if err := store.SaveLocked(); err != nil {
			cfg.Channels = oldChannels
			mu.Unlock()
			jsonError(rw, "failed to save config", http.StatusInternalServerError)
			return
		}
		mu.Unlock()

		if onChannelChange != nil {
			onChannelChange()
		}

		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/resolve-channel
	r.With(limitedBy(rl)).Post("/api/resolve-channel", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		input := strings.TrimSpace(body.Input)
		if input == "" {
			jsonError(rw, "input required", http.StatusBadRequest)
			return
		}

		resolved, err := utils.ResolveChannelInput(req.Context(), input)
		if err != nil {
			jsonError(rw, "failed to resolve channel", http.StatusUnprocessableEntity)
			return
		}

		if resolved == nil {
			// Not a recognized URL — return input as-is. Audit R-11:
			// flag resolved=false so callers don't mistake the echoed
			// input for a real lookup result.
			jsonResponse(rw, map[string]any{"id": input, "name": "", "platform": "", "resolved": false})
			return
		}

		jsonResponse(rw, map[string]any{
			"id":       resolved.ID,
			"name":     resolved.Name,
			"platform": resolved.Platform,
			"resolved": true,
		})
	})
}

// normalizeChannelID is utils.NormalizeChannelID behind a variable, so the
// route tests can answer a handle's lookup without reaching youtube.com.
var normalizeChannelID = utils.NormalizeChannelID

// channelIDRefusal words utils.NormalizeChannelID's refusal for an API
// response: an input that names no channel is the caller's mistake (400);
// a lookup that failed — YouTube unreachable, the handle's page gone — is
// the 422 POST /api/resolve-channel answers for the same failure.
func channelIDRefusal(err error) (string, int) {
	if errors.Is(err, utils.ErrNotChannelURL) {
		return utils.ErrNotChannelURL.Error(), http.StatusBadRequest
	}
	return "failed to resolve channel", http.StatusUnprocessableEntity
}

// applyResolvedChannel writes NormalizeChannelID's answer onto the channel
// being saved: its ID always; the resolved display name only where none
// was given; the resolved platform whenever resolution found one — a
// twitch.tv URL is a Twitch channel whatever the form said.
func applyResolvedChannel(ch *config.ChannelConfig, resolved *utils.ResolvedChannel) {
	ch.ID = resolved.ID
	if strings.TrimSpace(ch.Name) == "" && resolved.Name != "" {
		ch.Name = resolved.Name
	}
	if resolved.Platform != "" {
		ch.Platform = resolved.Platform
	}
}

// validChannelPlatform reports whether p is a channel platform the monitors
// know. Empty means YouTube. Shared by POST /api/config/channels and
// PUT /api/config's channels[] check so the two writers cannot disagree.
func validChannelPlatform(p string) bool {
	switch p {
	case "", "youtube", "twitch":
		return true
	}
	return false
}

// pathParam returns the named chi URL parameter DECODED. chi matches routes
// against r.URL.RawPath whenever Go kept one — which it does whenever the
// client's escaping differs from Go's own, as encodeURIComponent's does for
// '@' and ':' — so the parameter arrives still escaped: the dashboard's
// DELETE of "@SomeHandle" or of a URL-shaped ID matched no channel and
// answered 404. Only that case is decoded. Without a RawPath chi matched the
// already-decoded Path, and decoding again would turn a literal '%' in an
// ID into a wrong byte or an error.
func pathParam(req *http.Request, key string) string {
	v := chi.URLParam(req, key)
	if req.URL.RawPath == "" {
		return v
	}
	if dec, err := url.PathUnescape(v); err == nil {
		return dec
	}
	return v
}
