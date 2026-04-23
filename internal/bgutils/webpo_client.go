package bgutils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// WebPoClient orchestrates the full PO token generation flow:
// challenge -> BotGuard snapshot -> GenerateIT -> integrity token.
type WebPoClient struct {
	config *BgConfig
	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewWebPoClient creates a new WebPoClient.
func NewWebPoClient(config *BgConfig, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *WebPoClient {
	return &WebPoClient{config: config, logger: logger}
}

// GenerateTokenMinter performs the full flow and returns a minter that can produce PO tokens.
// Flow: fetch challenge -> create BotGuard VM -> take snapshot -> POST GenerateIT -> create minter.
//
// IMPORTANT: The returned TokenMinter holds closures over the BotGuard Goja runtime.
// The VM must stay alive for the lifetime of the minter. Call TokenMinter.Cleanup
// when evicting the minter from cache to shut down the VM.
func (wpc *WebPoClient) GenerateTokenMinter(ctx context.Context) (*TokenMinter, error) {
	wpc.logger.Debug("[PotProvider] Fetching BotGuard challenge...")

	// Step 1: Fetch challenge
	challenge, err := FetchChallenge(ctx, wpc.config)
	if err != nil {
		return nil, fmt.Errorf("fetch challenge: %w", err)
	}

	scriptLen := len(challenge.InterpreterScript)
	wpc.logger.Debug("[PotProvider] Challenge received",
		"globalName", challenge.GlobalName,
		"hasInterpreterURL", challenge.InterpreterURL != "",
		"interpreterURL", challenge.InterpreterURL,
		"interpreterScriptLen", scriptLen,
		"programLen", len(challenge.Program))

	// Step 2: Create BotGuard client
	wpc.logger.Debug("[PotProvider] Creating BotGuard client...")
	bgClient, err := NewBotGuardClient(ctx, challenge)
	if err != nil {
		return nil, fmt.Errorf("create BotGuard client: %w", err)
	}
	// NOTE: Do NOT defer bgClient.Shutdown() here!
	// The minter callback is a closure over the Goja VM state. Shutting down
	// the VM destroys globalThis[globalName] and DOM globals that the closure
	// references. The VM must stay alive for the entire minter lifetime.
	// Cleanup is deferred to TokenMinter.Cleanup (called on cache eviction).
	wpc.logger.Debug("[PotProvider] BotGuard client created")

	// Step 3: Take snapshot — creates a native JS array that BotGuard populates
	wpc.logger.Debug("[PotProvider] Generating BotGuard snapshot...")
	botguardResponse, webPoSignalOutput, err := bgClient.Snapshot(SnapshotTimeout)
	if err != nil {
		bgClient.Shutdown()
		return nil, fmt.Errorf("snapshot: %w", err)
	}

	// Step 3.5: Clear telemetry timers (safe — only monitoring, not functional)
	// BotGuard creates setInterval timers that leak ~1MB/30s if not cleared.
	bgClient.ClearTimers()

	// Dump JS console messages to diagnose BotGuard errors
	if consoleVal, err2 := bgClient.vm.RunString("typeof __consoleMessages !== 'undefined' ? JSON.stringify(__consoleMessages) : '[]'"); err2 == nil {
		msgs := consoleVal.String()
		if msgs != "[]" {
			wpc.logger.Debug("[PotProvider] BotGuard console output", "messages", msgs)
		}
	}

	// Log snapshot diagnostics
	responsePrefix := botguardResponse
	if len(responsePrefix) > 80 {
		responsePrefix = responsePrefix[:80] + "..."
	}
	wpc.logger.Debug("[PotProvider] BotGuard snapshot result",
		"responseLen", len(botguardResponse),
		"responsePrefix", responsePrefix)

	// Check if webPoSignalOutput was populated by BotGuard
	wpoLen := webPoSignalOutput.Get("length")
	wpc.logger.Debug("[PotProvider] webPoSignalOutput after snapshot",
		"length", wpoLen,
		"has0", webPoSignalOutput.Get("0") != nil && webPoSignalOutput.Get("0").String() != "undefined")

	wpc.logger.Debug("[PotProvider] Generating integrity token...")

	// Step 4: POST GenerateIT to get integrity token
	itData, err := generateIntegrityToken(ctx, wpc.config, botguardResponse)
	if err != nil {
		bgClient.Shutdown()
		return nil, fmt.Errorf("generate integrity token: %w", err)
	}

	// Step 5: Create minter — two paths depending on what GenerateIT returned.
	//
	// Path A (full): IntegrityToken present + webPoSignalOutput[0] populated
	//   → Use getMinter(integrityToken) → mintCallback → produces per-binding tokens
	//   → VM must stay alive for cached minter lifetime
	//
	// Path B (fallback): IntegrityToken is null, websafeFallbackToken provided
	//   → BotGuard couldn't fully verify environment (Goja VM without full DOM)
	//   → Google provides a pre-generated fallback token usable as PO token
	//   → No VM needed, can clean up immediately

	hasWebPoCallback := webPoSignalOutput.Get("0") != nil &&
		webPoSignalOutput.Get("0").String() != "undefined"

	if itData.IntegrityToken != "" && hasWebPoCallback {
		// Path A: Full minter flow
		minter, err := NewWebPoMinter(itData, webPoSignalOutput, bgClient.vm)
		if err != nil {
			bgClient.Shutdown()
			return nil, fmt.Errorf("create minter: %w", err)
		}

		tokenMinter := &TokenMinter{
			MintFunc:  minter.MintAsWebsafeString,
			ExpiresAt: time.Now().Add(itData.EstimatedTTL),
			// Route Cleanup through the minter's own Shutdown so VM teardown
			// serializes with any in-flight Mint call on the same minter.
			Cleanup: func() { minter.Shutdown(bgClient.Shutdown) },
		}

		wpc.logger.Info("[PotProvider] Generated integrity token (full minter)", "ttl", itData.EstimatedTTL)
		return tokenMinter, nil
	}

	// Path B removed: previously, when GenerateIT returned a null integrity token
	// we treated WebsafeFallbackToken as a valid PO token and cached it. That masked
	// BotGuard VM failures (e.g. crypto.getRandomValues regressions) and diverged from
	// upstream bgutil-ytdlp-pot-provider which errors in this case. YouTube does not
	// accept the websafe fallback as a real PO token for authenticated player requests,
	// so the silent-fallback behaviour degraded quality without signal.
	bgClient.Shutdown()
	if itData.WebsafeFallbackToken != "" {
		return nil, &BGError{
			Code:    ErrIntegrity,
			Message: "GenerateIT returned no integrity token (only websafe fallback); BotGuard VM likely failed — check goja shims",
		}
	}
	return nil, &BGError{Code: ErrIntegrity, Message: "no integrity token or fallback token available"}
}

// generateIntegrityToken posts the BotGuard response to the GenerateIT API.
func generateIntegrityToken(ctx context.Context, config *BgConfig, botguardResponse string) (*IntegrityTokenData, error) {
	url := GoogleWaaGenerateITURL
	if config.UseYouTubeAPI {
		url = YouTubeJnnGenerateITURL
	}

	body, err := json.Marshal([]any{config.RequestKey, botguardResponse})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json+protobuf")
	req.Header.Set("x-user-agent", "grpc-web-javascript/0.1")
	if !config.UseYouTubeAPI {
		req.Header.Set("x-goog-api-key", BotGuardAPIKey)
	}
	req.Header.Set("User-Agent", UserAgentShort)

	resp, err := bgHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST GenerateIT: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &BGError{
			Code:    ErrIntegrity,
			Message: fmt.Sprintf("GenerateIT returned status %d", resp.StatusCode),
		}
	}

	// Limit read to 1MB — integrity token responses are small
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return parseIntegrityTokenResponse(respBody)
}

// parseIntegrityTokenResponse parses the GenerateIT response.
// Response format: [integrityToken, estimatedTtlSecs, mintRefreshThreshold, websafeFallbackToken]
//
// When BotGuard can't fully verify the environment (e.g. Goja VM without full DOM),
// Google returns null for the integrity token but provides a websafeFallbackToken
// that can be used directly as a PO token.
func parseIntegrityTokenResponse(raw []byte) (*IntegrityTokenData, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if len(arr) < 2 {
		return nil, &BGError{Code: ErrIntegrity, Message: "GenerateIT response too short"}
	}

	// Parse integrity token (index 0) — may be null
	var token string
	if err := json.Unmarshal(arr[0], &token); err != nil {
		// null unmarshals to "" for strings, but if it's truly unparseable, ignore
		token = ""
	}

	var ttlSecs float64
	if err := json.Unmarshal(arr[1], &ttlSecs); err != nil {
		ttlSecs = 3600 // Default 1 hour
	}

	data := &IntegrityTokenData{
		IntegrityToken: token,
		EstimatedTTL:   time.Duration(ttlSecs) * time.Second,
	}

	// Parse optional fields: mintRefreshThreshold (index 2), websafeFallbackToken (index 3)
	if len(arr) > 2 {
		var threshold float64
		if json.Unmarshal(arr[2], &threshold) == nil {
			data.MintRefreshThreshold = int(threshold)
		}
	}
	if len(arr) > 3 {
		var fallback string
		if json.Unmarshal(arr[3], &fallback) == nil {
			data.WebsafeFallbackToken = fallback
		}
	}

	// Must have either an integrity token or a fallback token
	if token == "" && data.WebsafeFallbackToken == "" {
		return nil, &BGError{Code: ErrIntegrity, Message: "no integrity token or fallback token in response"}
	}

	return data, nil
}
