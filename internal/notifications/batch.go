package notifications

import (
	"fmt"
	"sync"
	"time"
)

// batchWindow is how long a target's coalescing window stays open, measured
// from the FIRST item in it — not a sliding window, so a steady trickle of
// finds cannot hold a message open indefinitely.
const batchWindow = 5 * time.Second

// maxEmbedsPerMessage is Discord's documented per-message embed cap
// (resources/message.mdx). An eleventh embed is a 400, and discord.go's ladder
// treats a non-429 4xx as permanent — so the eleventh embed does not cost
// itself, it costs the whole message.
const maxEmbedsPerMessage = 10

// emitFunc hands ONE Message to a target's FIFO queue as a single queue item,
// so the queue's drop policy and the ordering between messages are unchanged
// by batching.
type emitFunc func(msg Message)

// batchTimer is the only thing the batcher ever does to an armed timer: disarm
// it. An interface rather than *time.Timer so tests drive the window by hand.
type batchTimer interface{ Stop() bool }

// batchClock is the time source, injected so tests do not sleep.
type batchClock interface {
	AfterFunc(d time.Duration, f func()) batchTimer
}

// realBatchClock is what production uses.
type realBatchClock struct{}

func (realBatchClock) AfterFunc(d time.Duration, f func()) batchTimer {
	return time.AfterFunc(d, f)
}

// batcher coalesces batchable embeds for ONE target.
//
// It sits in FRONT of that target's FIFO (queue.go) and hands it whole
// Messages, so everything the queue does — the cap, the drop policy, the single
// draining goroutine, the order between messages — is unchanged by batching.
// What changes is how many embeds one queue item carries: a backfill re-scan
// that used to enqueue 200 separate `found`s (and shed most of them at the cap)
// now enqueues 20 ten-embed messages.
type batcher struct {
	window time.Duration
	clock  batchClock
	emit   emitFunc
	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// mu guards everything below. It is NEVER held across an emit: emit reaches
	// q.enqueue, which takes the queue's own mutex, and holding both would
	// invert the lock order against Add.
	mu      sync.Mutex
	pending []Embed
	// The single ping for the open window: the FIRST non-empty one handed in.
	// Discord applies content and allowed_mentions per MESSAGE, so there is
	// nowhere to put a second, and ten pings for one sweep is the noise
	// batching exists to remove.
	mention        string
	mentionAllowed *AllowedMentions
	// timer is armed by the first item of a window and disarmed by the flush.
	timer batchTimer
	// mode is the target's delivery mode (lifecycle.go), set at construction
	// by newTargetQueue and rebound by applyTargets through setMode on every
	// config load. ModeEdit turns this whole stage off for the target: an
	// edited message is ONE job's embed rewritten in place, so it can never
	// share a POST with another job's. The zero value reads as ModeSeparate,
	// which is what a bare newBatcher gets.
	mode string

	// betweenModeCheckAndAppend is a TEST-ONLY hook, nil in production: Add
	// calls it (with mu NOT held) exactly between its edit-mode check and the
	// append's lock section, so a test can land a setMode in that gap
	// deterministically.
	betweenModeCheckAndAppend func()
}

func newBatcher(window time.Duration, clock batchClock, emit emitFunc, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
},
) *batcher {
	if clock == nil {
		clock = realBatchClock{}
	}
	return &batcher{window: window, clock: clock, emit: emit, logger: logger}
}

// Add routes one embed: a non-batchable one is emitted immediately as a
// one-embed Message; a batchable one joins the open window, arming it if it
// was closed. mention/mentionAllowed travel with the embed so a flush can
// decide the message's single ping.
func (b *batcher) Add(e Embed, mention string, allowed *AllowedMentions) {
	// Edit mode gates the whole stage, as the first thing Add does: an edited
	// message is ONE job's embed rewritten in place, so it can never share a
	// POST with another job's. Everything below is written as if the target is
	// separate-mode, which past this point it is — save for a flip landing in the
	// gap before the append, which the append's own hold re-checks.
	b.mu.Lock()
	editMode := b.mode == ModeEdit
	b.mu.Unlock()
	if editMode {
		b.emit(Message{Embeds: []Embed{e}, Mention: mention, MentionAllowed: allowed})
		return
	}

	if !isBatchable(e.Opts) {
		// Deliberately AHEAD of the embeds already coalesced: an error must not
		// wait 5 s behind a backfill sweep. This is the one ordering change
		// batching makes — for a single job it means an `error` can land before
		// that job's own `found` — and operations.md names it rather than
		// claiming ordering is untouched.
		b.emit(Message{Embeds: []Embed{e}, Mention: mention, MentionAllowed: allowed})
		return
	}

	if b.betweenModeCheckAndAppend != nil {
		b.betweenModeCheckAndAppend()
	}
	b.mu.Lock()
	if b.mode == ModeEdit {
		// Re-checked under the hold that appends: a setMode(ModeEdit) landing
		// between the check above and this lock has already released the
		// window, so appending here would arm a fresh one on an edit-mode
		// target and hold this embed the full 5 s. Emit it at once instead,
		// with mu released first (emit is never called under it).
		b.mu.Unlock()
		b.emit(Message{Embeds: []Embed{e}, Mention: mention, MentionAllowed: allowed})
		return
	}
	defer b.mu.Unlock()
	b.pending = append(b.pending, e)
	if b.mention == "" && mention != "" && allowed != nil {
		b.mention, b.mentionAllowed = mention, allowed
	}
	if len(b.pending) > 1 {
		// The window is already open and its timer already armed. Re-arming
		// here would make it a SLIDING window, and a steady trickle of finds
		// would then hold the message open for as long as the trickle lasted.
		return
	}
	b.timer = b.clock.AfterFunc(b.window, func() {
		// time.AfterFunc runs its callback on its own goroutine, so it carries
		// the package's inline recover: a panic here would otherwise take the
		// process down from a timer nobody is watching.
		defer func() {
			if r := recover(); r != nil {
				b.logger.Error("panic in notification batch flush", "panic", fmt.Sprint(r))
			}
		}()
		b.Flush()
	})
}

// Flush emits every pending embed now. Called before closeDrain, and from
// applyTargets when a target is retired.
func (b *batcher) Flush() {
	b.mu.Lock()
	if b.timer != nil {
		// Disarm before releasing the window, so a timer that has not fired yet
		// cannot emit the same embeds a second time, and the NEXT window gets
		// its own full five seconds instead of inheriting what is left of this
		// one's.
		b.timer.Stop()
		b.timer = nil
	}
	pending := b.pending
	mention, allowed := b.mention, b.mentionAllowed
	b.pending, b.mention, b.mentionAllowed = nil, "", nil
	b.mu.Unlock()

	if len(pending) == 0 {
		return
	}
	// OUTSIDE b.mu — emit reaches q.enqueue and its mutex. Every message this
	// window splits into carries the ping, because content and allowed_mentions
	// are per message: withholding it from the second and third would drop the
	// alert's ping onto an arbitrary tenth of the sweep.
	for _, embeds := range splitMessages(pending) {
		b.emit(Message{Embeds: embeds, Mention: mention, MentionAllowed: allowed})
	}
}

// Stop flushes and disarms. A retired target calls it so the window it was
// holding reaches its queue instead of vanishing with the batcher.
func (b *batcher) Stop() { b.Flush() }

// setMode swaps the target's delivery mode, releasing the open window first.
//
// A batcher holding pending `found` embeds when its target switches to edit
// mode has that window delivered on the flip rather than dropped: a one-embed
// window goes out through the already-rebound dispatch and becomes that job's
// lifecycle message; a multi-embed window posts plain (dispatchOne's
// single-embed guard) — the same reason applyTargets flushes a retired
// target's window rather than letting it evaporate. Releasing the window on
// the way back to separate mode costs nothing (it is empty, because edit mode
// never opened one) and keeps the rule one sentence long.
//
// The window and the mode move under ONE hold, and the emit happens after it:
// an Add landing between a flush and a later swap would otherwise join a
// window that had just been emptied, arm its timer under the mode the flip is
// leaving, and make that embed wait out the full 5 s on a target that is now
// in edit mode — where a later event emitting at once would overtake it and
// append the job's History out of order.
//
// Both sides are normalised, so the first bind of a fresh batcher (already
// born in its target's mode — see newTargetQueue) is a no-op.
func (b *batcher) setMode(mode string) {
	mode = normalizeTargetMode(mode)
	b.mu.Lock()
	if normalizeTargetMode(b.mode) == mode {
		b.mu.Unlock()
		return
	}
	if b.timer != nil {
		// Disarm before releasing the window, exactly as Flush does: a timer
		// that has not fired yet must not emit these embeds a second time.
		b.timer.Stop()
		b.timer = nil
	}
	pending, mention, allowed := b.pending, b.mention, b.mentionAllowed
	b.pending, b.mention, b.mentionAllowed = nil, "", nil
	b.mode = mode
	b.mu.Unlock()

	// OUTSIDE b.mu — emit reaches q.enqueue and the queue's own mutex.
	for _, embeds := range splitMessages(pending) {
		b.emit(Message{Embeds: embeds, Mention: mention, MentionAllowed: allowed})
	}
}

// isBatchable reports whether a send coalesces.
//
// The closed set is deliberately small: the families where ONE operation
// produces dozens of embeds. Everything else — `error`, `cancelled`, the
// `trim_*` family, every System event, and an unfiltered send with no event at
// all — goes out at once, because a five-second wait is exactly wrong for an
// alert.
func isBatchable(opts SendOptions) bool {
	switch opts.Event {
	case "found", "added":
		return true
	case "auth":
		// The per-JOB "Authentication Required" is the burst: one dead cookie
		// parks N jobs. The platform-level cookie family carries no JobID and
		// is already cooldown-deduped, so making it wait would delay the only
		// alert that tells an operator their credentials are gone.
		return opts.JobID != ""
	default:
		return false
	}
}

// splitMessages chops pending embeds into messages that satisfy BOTH of
// Discord's per-message caps — at most maxEmbedsPerMessage embeds, and at
// most limitTotal characters summed across them — preserving arrival order.
func splitMessages(pending []Embed) [][]Embed {
	if len(pending) == 0 {
		return nil
	}
	out := make([][]Embed, 0, (len(pending)+maxEmbedsPerMessage-1)/maxEmbedsPerMessage)
	start, runes := 0, 0
	for i := range pending {
		size := embedSize(pending[i])
		// Either cap closes the current message and this embed opens the next.
		// clampEmbed holds a SINGLE embed to limitTotal, so one embed always
		// fits on its own and a full-size arrival can never spin here.
		if i > start && (i-start == maxEmbedsPerMessage || runes+size > limitTotal) {
			out = append(out, pending[start:i:i])
			start, runes = i, 0
		}
		runes += size
	}
	return append(out, pending[start:len(pending):len(pending)])
}

// embedSize is one CLAMPED embed's contribution to the per-MESSAGE character
// total Discord applies its 6000 against. It goes through toDiscordEmbed
// (discord.go), the same conversion buildPayload will use, so the size
// splitMessages measures is the size the payload will have. Sizing each embed
// once is also what lets the splitter accumulate a running total instead of
// re-summing the message it is building; the test-only sum over a whole
// message lives in batch_test.go.
func embedSize(e Embed) int {
	de := toDiscordEmbed(e)
	return embedRunes(&de)
}

// batchIsLowTier reports whether EVERY member is low-tier.
//
// One normal-tier member protects the whole message from the queue's
// drop-oldest-low-tier policy: a ten-embed batch that included an
// "Authentication Required" must not be shed ahead of a lone Stream Found.
func batchIsLowTier(embeds []Embed) bool {
	if len(embeds) == 0 {
		return false
	}
	for i := range embeds {
		if effectiveTier(embeds[i].Opts) != TierLow {
			return false
		}
	}
	return true
}
