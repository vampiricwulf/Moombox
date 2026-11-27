package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// twitchLoginRe is what a Twitch login may be; anything else from the reply
// is not passed on to a test as a channel.
var twitchLoginRe = regexp.MustCompile(`^[a-z0-9_]{2,25}$`)

// runPickTwitchLive prints the login of a channel that is live now, from
// Twitch's anonymous top-streams query (no cookie, the public web Client-ID
// Moombox itself uses), so the channel tests need no setting. The top stream
// is the one least likely to end mid-run.
func runPickTwitchLive(out io.Writer) error {
	body := []byte(`{"query":"query{streams(first:5){edges{node{type broadcaster{login}}}}}"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, constants.TwitchURLs.GQL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Client-ID", constants.TwitchGQLClientID)
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("top-streams query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("top-streams query: status %d", resp.StatusCode)
	}
	var reply struct {
		Data struct {
			Streams struct {
				Edges []struct {
					Node struct {
						Type        string `json:"type"`
						Broadcaster struct {
							Login string `json:"login"`
						} `json:"broadcaster"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"streams"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply); err != nil {
		return fmt.Errorf("top-streams query: %w", err)
	}
	for _, e := range reply.Data.Streams.Edges {
		if e.Node.Type == "live" && twitchLoginRe.MatchString(e.Node.Broadcaster.Login) {
			fmt.Fprintln(out, e.Node.Broadcaster.Login)
			return nil
		}
	}
	return errors.New("top-streams query named no live channel")
}
