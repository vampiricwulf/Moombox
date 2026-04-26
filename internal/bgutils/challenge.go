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

	"github.com/vampiricwulf/Moombox/internal/httpx"
)

// bgHTTPClient is a dedicated HTTP client for BotGuard API requests.
// Backed by the shared httpx.Client transport so keep-alive
// amortisation works across packages that hit jnn-pa.googleapis.com.
var bgHTTPClient = httpx.Client(30 * time.Second)

// FetchChallenge fetches and descrambles a BotGuard challenge from the WAA API.
// logger is optional (nil-safe) and receives a debug summary of which challenge
// indices produced values — see reports/bgutils.md QI-9 for rationale (silent
// failures in extractStringFromArrayOrValue make upstream format drift hard
// to diagnose).
func FetchChallenge(ctx context.Context, config *BgConfig, logger botguardLogger) (*DescrambledChallenge, error) {
	if config.RequestKey == "" {
		return nil, &BGError{Code: ErrBadConfig, Message: "requestKey is required"}
	}

	// Always use the Google WAA endpoint; the YouTube JNN alternative
	// existed as scaffolding for a never-shipped owner toggle and added
	// only complexity. Audit bgutils DEAD-5.
	url := GoogleWaaCreateURL

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
	req.Header.Set("x-goog-api-key", BotGuardAPIKey)
	req.Header.Set("User-Agent", UserAgentFull)

	resp, err := bgHTTPClient.Do(req)
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

	challenge, err := parseChallengeData(respBody)
	if err != nil {
		return nil, err
	}
	// Diagnostic: which indices yielded usable values? extractStringFromArrayOrValue
	// silently returns "" on parse failure, so upstream format drift would otherwise
	// surface only as a generic "no interpreter URL or script" error downstream.
	if logger != nil {
		logger.Debug("[PotProvider] challenge indices populated",
			"hasMessageID", challenge.MessageID != "",
			"hasInterpreterURL", challenge.InterpreterURL != "",
			"hasInterpreterScript", challenge.InterpreterScript != "",
			"hasInterpreterHash", challenge.InterpreterHash != "",
			"hasProgram", challenge.Program != "",
			"hasGlobalName", challenge.GlobalName != "",
		)
	}
	return challenge, nil
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

	// Challenge array indices (from BgUtils reference implementation).
	// Named constants keep the index-based parsing self-documenting and make
	// future format drift easy to spot (a new index lands → add a const).
	const (
		idxMessageID         = 0 // request tracking
		idxInterpreterScript = 1 // inline JS (wrappedScript; fallback when URL absent)
		idxInterpreterURL    = 2 // URL to fetch interpreter JS (primary source)
		idxInterpreterHash   = 3 // integrity check
		idxProgram           = 4 // BotGuard bytecode (required)
		idxGlobalName        = 5 // VM global object name (required)
		// idx 6 unused / reserved
		// idx 7 clientExperimentsStateBlob — parsed-but-unused historically;
		// dropped in DEAD-6. If a future caller needs it, add a const + field.
	)

	if len(challengeArr) > idxMessageID {
		var messageID string
		if err := json.Unmarshal(challengeArr[idxMessageID], &messageID); err == nil {
			challenge.MessageID = messageID
		}
	}

	if len(challengeArr) > idxProgram {
		var program string
		if err := json.Unmarshal(challengeArr[idxProgram], &program); err == nil {
			challenge.Program = program
		}
	}

	if len(challengeArr) > idxGlobalName {
		var globalName string
		if err := json.Unmarshal(challengeArr[idxGlobalName], &globalName); err == nil {
			challenge.GlobalName = globalName
		}
	}

	// Inline script (fallback) — extract first, then use only if URL absent.
	var interpreterScript string
	if len(challengeArr) > idxInterpreterScript {
		interpreterScript = extractStringFromArrayOrValue(challengeArr[idxInterpreterScript])
	}

	// URL (primary source).
	if len(challengeArr) > idxInterpreterURL {
		url := extractStringFromArrayOrValue(challengeArr[idxInterpreterURL])
		if url != "" {
			challenge.InterpreterURL = url
		}
	}
	if challenge.InterpreterURL == "" && interpreterScript != "" {
		challenge.InterpreterScript = interpreterScript
	}

	if len(challengeArr) > idxInterpreterHash {
		var hash string
		if err := json.Unmarshal(challengeArr[idxInterpreterHash], &hash); err == nil {
			challenge.InterpreterHash = hash
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
