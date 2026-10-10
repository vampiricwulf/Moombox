package twitch

import "time"

// chatDelays is every timing knob the IRC session sleeps on or measures
// against, in one place so tests can drive the same loop at millisecond scale.
// Production values are the package constants named beside each field (pinned
// by TestDefaultChatDelaysMatchConstants), so the constants remain the
// documentation of intent and this struct is the only knob. Tests assign it
// directly after NewChatDownloader, the way internal/engine's tests poke
// SegmentDownloader.delays; nothing outside the package sees it.
//
// ircReadDeadline is NOT here: no test waits on it, and it is the outer bound
// rather than a loop wait.
type chatDelays struct {
	keepaliveIdle     time.Duration // ircKeepaliveIdle — silence before we speak first
	keepalivePongWait time.Duration // ircKeepalivePongWait — how long an answer may take
	keepaliveCheck    time.Duration // ircKeepaliveCheck — how often the two above are evaluated
	resumeSaveFloor   time.Duration // ircResumeSaveFloor — minimum gap between resume-sidecar writes
	reconnectBase     time.Duration // ircReconnectBase — first step of the reconnect backoff
	reconnectCap      time.Duration // ircReconnectCap — the backoff's ceiling
	exhaustedRetry    time.Duration // ircExhaustedRetry — the cadence once the reconnect budget is spent
	partBaseWait      time.Duration // ircPartBaseWait — how long a part's chat waits for its video's first segment
}

// defaultChatDelays returns production timing.
func defaultChatDelays() chatDelays {
	return chatDelays{
		keepaliveIdle:     ircKeepaliveIdle,
		keepalivePongWait: ircKeepalivePongWait,
		keepaliveCheck:    ircKeepaliveCheck,
		resumeSaveFloor:   ircResumeSaveFloor,
		reconnectBase:     ircReconnectBase,
		reconnectCap:      ircReconnectCap,
		exhaustedRetry:    ircExhaustedRetry,
		partBaseWait:      ircPartBaseWait,
	}
}
