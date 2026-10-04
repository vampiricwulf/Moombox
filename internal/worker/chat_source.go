package worker

import "context"

// ChatSource is the unified lifecycle interface for chat downloaders across
// platforms. Implementations:
//   - *chat.ChatDownloader   — YouTube live + replay polling
//   - *twitch.ChatDownloader — Twitch IRC live
//   - *twitch.VodChatDownloader — Twitch VOD via GQL pagination
//
// MarkStreamEnded semantics differ per platform:
//   - YouTube: signals "stream ended; drain remaining and exit cleanly"
//   - Twitch IRC: a clean end, unlike Stop — drains, clears the resume state
//     and runs the post-capture enrichment (twitch.ChatDownloader.MarkStreamEnded)
//   - Twitch VOD: no-op (pagination terminates on hasNextPage=false from server)
//
// Audit reports/chat.md T2 + reports/twitch.md #20.
type ChatSource interface {
	Start(ctx context.Context) error
	Stop()
	MarkStreamEnded()
	MessageCount() int
	IsRunning() bool
}
