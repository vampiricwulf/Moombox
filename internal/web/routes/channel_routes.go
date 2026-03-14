package routes

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// ChannelRoutes registers channel-related API routes.
// cfgMu protects concurrent reads/writes to the shared cfg struct.
func ChannelRoutes(r chi.Router, cfg *config.MoomboxConfig, cfgMu *sync.RWMutex, saveConfig func(*config.MoomboxConfig) error, onChannelChange func()) {
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

		// Upsert
		cfgMu.Lock()
		oldChannels := make([]config.ChannelConfig, len(cfg.Channels))
		copy(oldChannels, cfg.Channels)
		found := false
		for i, ch := range cfg.Channels {
			if ch.ID == channel.ID {
				cfg.Channels[i] = channel
				found = true
				break
			}
		}
		if !found {
			cfg.Channels = append(cfg.Channels, channel)
		}

		// Persist to disk
		if saveConfig != nil {
			if err := saveConfig(cfg); err != nil {
				cfg.Channels = oldChannels
				cfgMu.Unlock()
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}
		cfgMu.Unlock()

		if onChannelChange != nil {
			onChannelChange()
		}

		jsonResponse(rw, map[string]any{"success": true, "channel": channel})
	})

	// DELETE /api/config/channels/:id
	r.Delete("/api/config/channels/{id}", func(rw http.ResponseWriter, req *http.Request) {
		channelID := chi.URLParam(req, "id")

		cfgMu.Lock()
		oldChannels := make([]config.ChannelConfig, len(cfg.Channels))
		copy(oldChannels, cfg.Channels)
		found := false
		for i, ch := range cfg.Channels {
			if ch.ID == channelID {
				cfg.Channels = append(cfg.Channels[:i], cfg.Channels[i+1:]...)
				found = true
				break
			}
		}

		if !found {
			cfgMu.Unlock()
			jsonError(rw, "channel not found", http.StatusNotFound)
			return
		}

		// Persist to disk
		if saveConfig != nil {
			if err := saveConfig(cfg); err != nil {
				cfg.Channels = oldChannels
				cfgMu.Unlock()
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}
		cfgMu.Unlock()

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

		cfgMu.Lock()

		if len(body.IDs) != len(cfg.Channels) {
			cfgMu.Unlock()
			jsonError(rw, "ids count must match channels count", http.StatusBadRequest)
			return
		}

		// Reject duplicate IDs
		seen := make(map[string]bool, len(body.IDs))
		for _, id := range body.IDs {
			if seen[id] {
				cfgMu.Unlock()
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
				cfgMu.Unlock()
				jsonError(rw, "unknown channel ID: "+id, http.StatusBadRequest)
				return
			}
			reordered = append(reordered, ch)
		}

		oldChannels := cfg.Channels
		cfg.Channels = reordered

		if saveConfig != nil {
			if err := saveConfig(cfg); err != nil {
				cfg.Channels = oldChannels
				cfgMu.Unlock()
				jsonError(rw, "failed to save config", http.StatusInternalServerError)
				return
			}
		}
		cfgMu.Unlock()

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
			// Not a recognized URL — return input as-is
			jsonResponse(rw, map[string]any{"id": input, "name": "", "platform": ""})
			return
		}

		jsonResponse(rw, map[string]any{
			"id":       resolved.ID,
			"name":     resolved.Name,
			"platform": resolved.Platform,
		})
	})
}
