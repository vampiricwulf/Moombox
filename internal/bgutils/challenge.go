package bgutils

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// FetchChallenge fetches and descrambles a BotGuard challenge from the WAA API.
func FetchChallenge(ctx context.Context, config *BgConfig) (*DescrambledChallenge, error) {
	if config.RequestKey == "" {
		return nil, &BGError{Code: ErrBadConfig, Message: "requestKey is required"}
	}

	// Build request
	url := GoogleWaaCreateURL
	if config.UseYouTubeAPI {
		url = YouTubeJnnCreateURL
	}

	body, err := json.Marshal([]string{config.RequestKey})
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &BGError{Code: ErrRequestFailed, Message: fmt.Sprintf("fetch challenge: %v", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &BGError{
			Code:    ErrRequestFailed,
			Message: fmt.Sprintf("challenge API returned status %d", resp.StatusCode),
		}
	}

	// Limit read to 1MB — challenge responses are small
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return parseChallengeData(respBody)
}

// parseChallengeData parses the raw challenge response and descrambles the data.
func parseChallengeData(raw []byte) (*DescrambledChallenge, error) {
	// Response is a JSON array
	var rawArr []json.RawMessage
	if err := json.Unmarshal(raw, &rawArr); err != nil {
		return nil, &BGError{Code: ErrChallengeParse, Message: fmt.Sprintf("parse response array: %v", err)}
	}

	if len(rawArr) == 0 {
		return nil, &BGError{Code: ErrChallengeParse, Message: "empty challenge response"}
	}

	// Check if there's scrambled data to descramble (element at index 1)
	var challengeArr []json.RawMessage
	if len(rawArr) > 1 {
		var scrambled string
		if err := json.Unmarshal(rawArr[1], &scrambled); err == nil && scrambled != "" {
			descrambled, err := descramble(scrambled)
			if err != nil {
				return nil, &BGError{Code: ErrChallengeParse, Message: fmt.Sprintf("descramble: %v", err)}
			}
			if err := json.Unmarshal([]byte(descrambled), &challengeArr); err != nil {
				return nil, &BGError{Code: ErrChallengeParse, Message: fmt.Sprintf("parse descrambled data: %v", err)}
			}
		}
	}

	// If no descrambled data, use the raw array directly
	if challengeArr == nil {
		challengeArr = rawArr
	}

	challenge := &DescrambledChallenge{}

	// Index 0: messageId
	if len(challengeArr) > 0 {
		var messageID string
		if err := json.Unmarshal(challengeArr[0], &messageID); err == nil {
			challenge.MessageID = messageID
		}
	}

	// Extract fields from challenge array by index
	// Index 4: program (required)
	if len(challengeArr) > 4 {
		var program string
		if err := json.Unmarshal(challengeArr[4], &program); err == nil {
			challenge.Program = program
		}
	}

	// Index 5: globalName (required)
	if len(challengeArr) > 5 {
		var globalName string
		if err := json.Unmarshal(challengeArr[5], &globalName); err == nil {
			challenge.GlobalName = globalName
		}
	}

	// Index 1: wrappedScript (inline JS) - used as fallback
	var interpreterScript string
	if len(challengeArr) > 1 {
		interpreterScript = extractStringFromArrayOrValue(challengeArr[1])
	}

	// Index 2: wrappedUrl (URL to fetch interpreter JS) - primary source
	if len(challengeArr) > 2 {
		url := extractStringFromArrayOrValue(challengeArr[2])
		if url != "" {
			challenge.InterpreterURL = url
		}
	}
	// Fallback to inline script if no URL
	if challenge.InterpreterURL == "" && interpreterScript != "" {
		challenge.InterpreterScript = interpreterScript
	}

	// Index 3: interpreterHash
	if len(challengeArr) > 3 {
		var hash string
		if err := json.Unmarshal(challengeArr[3], &hash); err == nil {
			challenge.InterpreterHash = hash
		}
	}

	// Index 7: clientExperimentsStateBlob
	if len(challengeArr) > 7 {
		var blob string
		if err := json.Unmarshal(challengeArr[7], &blob); err == nil {
			challenge.ClientExperimentsBlob = blob
		}
	}

	if challenge.Program == "" || challenge.GlobalName == "" {
		return nil, &BGError{
			Code:    ErrChallengeParse,
			Message: "missing required fields: program or globalName",
		}
	}

	return challenge, nil
}

// descramble decodes a scrambled challenge string.
// Algorithm: base64decode -> each byte + 97 -> UTF-8 string -> JSON
func descramble(scrambled string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(scrambled)
	if err != nil {
		// Try URL-safe base64
		data, err = base64.RawURLEncoding.DecodeString(scrambled)
		if err != nil {
			return "", fmt.Errorf("base64 decode: %w", err)
		}
	}

	// Each byte + 97
	result := make([]byte, len(data))
	for i, b := range data {
		result[i] = b + 97
	}

	return string(result), nil
}

// extractStringFromArrayOrValue tries to extract a string from a JSON value
// that could be a string directly or an array containing strings.
func extractStringFromArrayOrValue(raw json.RawMessage) string {
	// Try as direct string
	var str string
	if err := json.Unmarshal(raw, &str); err == nil && str != "" {
		return str
	}

	// Try as array, find first string element
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, elem := range arr {
			var s string
			if err := json.Unmarshal(elem, &s); err == nil && s != "" {
				return s
			}
		}
	}

	return ""
}
