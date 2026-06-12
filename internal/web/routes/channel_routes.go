package routes

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// ChannelRoutes registers channel-related API routes. The Store carries
// the cfg pointer + lock; SaveLocked persists to disk under the same lock
// so a rollback can restore the in-memory channel slice if the save fails.
func ChannelRoutes(r chi.Router, store *config.Store, onChannelChange func()) {
	mu := store.RWMutex()
	cfg := store.Config()

	// POST /api/config/channels
	r.Post("/api/config/channels", func(rw http.ResponseWriter, req *http.Request) {
		var channel config.ChannelConfig
		if err := json.NewDecoder(req.Body).Decode(&channel); err != nil {
			jsonError(rw, "invalid channel config", http.StatusBadRequest)
			return
		}

		if channel.ID == "" {
			jsonError(rw, "channel ID required", http.StatusBadRequest)
			return
		}

		// Safety net: if the ID looks like a URL, try to resolve it
		if utils.LooksLikeURL(channel.ID) {
			resolved, err := utils.ResolveChannelInput(req.Context(), channel.ID)
			if err == nil && resolved != nil {
				channel.ID = resolved.ID
				if channel.Name == "" && resolved.Name != "" {
					channel.Name = resolved.Name
				}
				if resolved.Platform != "" {
					channel.Platform = resolved.Platform
				}
			}
		}

		// Upsert — copy-on-write: mutate a CLONE and assign the whole slice.
		// Store.Snapshot() readers (GET /api/config marshals after releasing
		// the lock) share the previous backing array, so writing an element
		// in place would race their reads; whole-slice replacement is the
		// documented Store contract. The old header doubles as the rollback
		// snapshot since its array is never touched.
		mu.Lock()
		oldChannels := cfg.Channels
		newChannels := slices.Clone(cfg.Channels)
		found := false
		for i, ch := range newChannels {
			if ch.ID == channel.ID {
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
	})

	// DELETE /api/config/channels/:id
	r.Delete("/api/config/channels/{id}", func(rw http.ResponseWriter, req *http.Request) {
		channelID := chi.URLParam(req, "id")

		// Copy-on-write for the same reason as the upsert above: the old
		// append-shift compacted elements inside the shared backing array,
		// racing lock-free Snapshot readers.
		mu.Lock()
		oldChannels := cfg.Channels
		idx := -1
		for i, ch := range cfg.Channels {
			if ch.ID == channelID {
				idx = i
				break
			}
		}
		if idx < 0 {
			mu.Unlock()
			jsonError(rw, "channel not found", http.StatusNotFound)
			return
		}
		newChannels := slices.Clone(cfg.Channels)
		newChannels = slices.Delete(newChannels, idx, idx+1)
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

		jsonResponse(rw, map[string]any{"success": true})
	})

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
	r.Post("/api/resolve-channel", func(rw http.ResponseWriter, req *http.Request) {
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
