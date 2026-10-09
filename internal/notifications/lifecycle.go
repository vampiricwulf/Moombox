package notifications

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Per-target delivery modes (the `mode` key on a [[notifications]] entry).
const (
	// ModeSeparate is the default: one message per event, never edited.
	ModeSeparate = "separate"
	// ModeEdit gives a job ONE message per target, created by its first
	// allowed lifecycle event and rewritten by every later one.
	ModeEdit = "edit"
)

// lifecycleEvents is the set that shares a job's one edited message (spec
// §4.2). Everything else — error, cancelled, auth, trim_* and all of System —
// is always its own post: those may carry a mention, and an edit does not
// re-notify anyone (per the Discord API docs an edit returns the updated
// message object and says nothing about re-notifying).
//
// TWO tests change when this set does: TestLifecycleEventSet
// (lifecycle_test.go) pins the membership, and
// TestEveryLifecycleSendCarriesItsJob (internal/worker,
// notify_lifecycle_ids_test.go) holds a hand copy — it is an AST guard over
// that package's producers, and widening this package's API for it would be
// the tail wagging the dog.
var lifecycleEvents = map[string]bool{
	"found":               true,
	"added":               true,
	"scheduled":           true,
	"rescheduled":         true,
	"downloading":         true,
	"quality_split":       true,
	"gap_split":           true,
	"connectivity_resume": true,
	"connectivity_split":  true,
	"muxing":              true,
	"finished":            true,
}

// terminalLifecycleEvents close a job's story. By owner ruling (2026-09-27)
// they do BOTH: edit the lifecycle message to its terminal look, then post the
// separate embed — which is the one that carries the mention. They never
// CREATE a lifecycle message; with no message open they are a plain post.
var terminalLifecycleEvents = map[string]bool{
	"error":     true,
	"cancelled": true,
}

// lifecycleLabels is the word an event puts in the Status field and in its
// History line. Short and neutral on purpose: the line is read in a column
// beside a relative timestamp.
var lifecycleLabels = map[string]string{
	"found":               "Found",
	"added":               "Added",
	"scheduled":           "Scheduled",
	"rescheduled":         "Rescheduled",
	"downloading":         "Downloading",
	"quality_split":       "Quality split",
	"gap_split":           "Gap split",
	"connectivity_resume": "Resumed",
	"connectivity_split":  "Connectivity split",
	"muxing":              "Muxing",
	"finished":            "Finished",
	"error":               "Failed",
	"cancelled":           "Cancelled",
}

const (
	// historyFieldMax bounds the History field value. Discord's own field
	// limit is 1024 (per the API docs, message.mdx) and buildPayload clamps
	// there; this sits under it so the clamp that bites is OURS — dropping
	// whole oldest lines — rather than a mid-line truncation.
	historyFieldMax = 1000
	// statusFieldName / historyFieldName are the two fields the rewrite adds.
	statusFieldName  = "Status"
	historyFieldName = "History"
)

// MessageStore persists a job's per-target lifecycle message ids. Implemented
// by *database.Database (NotificationMsgs / UpdateNotificationMsgs).
//
// An interface rather than the concrete type because internal/notifications
// imports no database package — and because the tests need a store they can
// pre-populate to stand in for "a restart with ids already on the row".
type MessageStore interface {
	// NotificationMsgs returns the stored target-key -> message-id map for a
	// job, or nil when there is none.
	NotificationMsgs(jobID string) map[string]string
	// UpdateNotificationMsgs replaces the stored map. Returns false when the
	// job row is gone.
	UpdateNotificationMsgs(jobID string, msgs map[string]string) bool
}

// targetMsgKey is the stable per-target key the persisted map is keyed on: the
// first 16 hex digits of SHA-256 over the RESOLVED webhook URL — the same
// value buildTargets dedupes targets on, so the two spellings of one webhook
// share one message.
//
// Hashed, not stored raw: the webhook path IS the credential, and this value
// lands in the database and in every backup of it. 64 bits is far past enough
// to separate the handful of webhooks one install configures.
func targetMsgKey(resolvedURL string) string {
	if resolvedURL == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(resolvedURL))
	return hex.EncodeToString(sum[:])[:16]
}

// normalizeTargetMode maps a configured mode onto the two the manager knows.
// Empty is the default; anything unrecognised is treated as the default too —
// config validation has already reported it, and a typo must not silently turn
// edit mode ON.
//
// Exact match, NOT case-insensitive: validateOrNormalize rejects "Edit" and
// rewrites it to "separate", so accepting it here would make the manager
// disagree with the value the config layer says is stored. Surrounding
// whitespace is tolerated, and the loader trims too — so " edit " reads the
// same on both sides.
func normalizeTargetMode(mode string) string {
	if strings.TrimSpace(mode) == ModeEdit {
		return ModeEdit
	}
	return ModeSeparate
}

// lifecycleTracker holds the per-(job, target) message ids and history lines.
//
// Ids are loaded from the store the first time a job is touched after boot and
// written back on the first successful POST for a pair — one silent write per
// (job, target), which is the whole budget the ruling allows.
//
// History is in-memory ONLY and per (job, target): a target with an event
// filter must see a history of what IT received, not of what the job did. It
// therefore restarts empty after a reboot — the message keeps being edited
// (that is what the persisted id buys) but its History begins again at the
// first post-restart state. Persisting the lines would need a second column
// and a write per event, which the cadence ruling does not allow.
// The map is bounded: entries are released when a job's story ends (see
// release) and, for jobs that never reach a terminal event, evicted
// least-recently-touched first past maxTrackedJobs — never while a send the
// entry is read for is still queued (hold).
type lifecycleTracker struct {
	// writeMu serialises the whole read-modify-write of a job's stored map
	// against the store. UpdateNotificationMsgs replaces the WHOLE map, so
	// two targets recording an id for the SAME job concurrently would
	// otherwise take independent snapshots and the later write could erase
	// the earlier target's id — the row must carry both. Held ACROSS the
	// store call and never while any other lock is taken but mu, which is
	// released before the write; mu is never held when this is acquired.
	writeMu sync.Mutex

	mu      sync.Mutex
	store   MessageStore
	jobs    map[string]*lifecycleJob
	touched map[string]uint64 // job id -> last-touch sequence, for eviction
	seq     uint64
	// dropping counts, per deleted job and key, the ForgetJob steps queued
	// for that key that have not run yet (markDropping). While a key has one,
	// whatever id it holds for the job is the deleted job's — even one its
	// own POST records after the delete — and it is never written to a row.
	//
	// Beside jobs, not inside an entry: release, eviction and another
	// target's drop delete entries, and a mark lost with one let the deleted
	// job's id land on a re-added job's row after all.
	dropping map[string]map[string]int
	// held counts, per job, the managed sends queued for it on an edit-mode
	// target and not yet gone from their queue (hold). evictLocked passes a
	// held job over: its row can be deleted before the send is dispatched —
	// deleting an active job queues its cancel first — and an entry evicted
	// meanwhile had nothing left to be rebuilt from.
	//
	// Beside jobs, like dropping: release and the drops delete entries a
	// queued send still holds, and the count must outlive them.
	held map[string]int
	log  interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

type lifecycleJob struct {
	loaded  bool
	msgs    map[string]string   // target key -> message id
	history map[string][]string // target key -> rendered history lines
	closed  map[string]bool     // target keys whose story this process has finished telling
}

// maxTrackedJobs is the backstop for a job that never reaches a terminal
// event. Past it the tracker drops its least-recently-touched entries; each
// one costs a single store read to rebuild, and the message it was editing is
// unaffected because the id is on the row — while the row is there, which is
// why a job with a managed send still queued is passed over (hold).
const maxTrackedJobs = 512

func newLifecycleTracker(store MessageStore) *lifecycleTracker {
	return &lifecycleTracker{
		store:    store,
		jobs:     map[string]*lifecycleJob{},
		touched:  map[string]uint64{},
		dropping: map[string]map[string]int{},
		held:     map[string]int{},
	}
}

// setStore attaches (or replaces) the persistence backend. Called by
// Manager.SetMessageStore, so wiring order at boot never matters.
func (l *lifecycleTracker) setStore(s MessageStore) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.store = s
}

// jobLocked returns the job's state, loading the persisted ids on first touch.
// Caller holds l.mu.
func (l *lifecycleTracker) jobLocked(jobID string) *lifecycleJob {
	l.seq++
	l.touched[jobID] = l.seq
	j := l.jobs[jobID]
	if j == nil {
		j = &lifecycleJob{msgs: map[string]string{}, history: map[string][]string{}}
		l.jobs[jobID] = j
		l.evictLocked(jobID)
	}
	if !j.loaded {
		j.loaded = true
		if l.store != nil {
			for k, v := range l.store.NotificationMsgs(jobID) {
				if k != "" && v != "" {
					j.msgs[k] = v
				}
			}
		}
	}
	return j
}

// extraMsg is a second message one target holds for a job: one an old
// spelling of its webhook opened (messageID), kept under that spelling's key.
type extraMsg struct {
	key, id string
}

// messageID returns the message this target edits for this job — "" when it
// has none yet — and any other message of the job's this target rewrites
// with it.
//
// legacy are the keys an older release stored this target's ids under
// (legacyResolvedURL, manager.go). A job whose row still holds one was opened
// before its webhook's spelling was canonicalised, and its next event must
// edit that message, not open a second one beside it. The id is ADOPTED: it
// moves to key in memory and the legacy key leaves the map, so the job's next
// row write (remember) stores it under the current key and drops the old one
// — no write of its own, so the one-write-per-(job, target) budget stands.
// Until then a restart reads the old key again and adopts it again. A legacy
// key holding nothing, or the id key already holds, leaves the map too:
// release closes the entry only once every key in the map is closed.
//
// A legacy key holding a DIFFERENT id is a second message: 2.8.9 and 2.8.10
// built one target per spelling, so a webhook configured as "…/TOKEN" and
// "…/TOKEN/" posted, and kept editing, one message per spelling for every
// job. Dropping it orphaned it, reading "Downloading" for good. It stays under
// its old key and is returned in extra: every edit rewrites it too, and the
// terminal edit closes it (release) like the job's own message.
func (l *lifecycleTracker) messageID(jobID, key string, legacy ...string) (id string, extra []extraMsg) {
	l.mu.Lock()
	defer l.mu.Unlock()
	j := l.jobLocked(jobID)
	for _, lk := range legacy {
		if lk == "" || lk == key {
			continue
		}
		switch old := j.msgs[lk]; {
		case old == "" || old == j.msgs[key]:
			delete(j.msgs, lk)
		case j.msgs[key] == "":
			j.msgs[key] = old
			delete(j.msgs, lk)
		default:
			extra = append(extra, extraMsg{key: lk, id: old})
		}
	}
	return j.msgs[key], extra
}

// remember records a newly created message and persists the job's whole map.
// A no-op when the id is already what we hold, so the ruling's "one write per
// job per target, on the first successful POST" holds even if a caller repeats.
//
// The mutation, the snapshot and the store write all happen under writeMu, as
// one read-modify-write: the column holds the whole map, so a second target
// recording an id for the same job at the same moment must see this one's id
// in ITS snapshot rather than replacing the row with a map that never had it.
//
// A failed write (the row was deleted mid-download) keeps the in-memory id:
// edits still work for the rest of this process, which is all a deleted job
// could want.
func (l *lifecycleTracker) remember(jobID, key, messageID string) {
	if jobID == "" || key == "" || messageID == "" {
		return
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	l.mu.Lock()
	j := l.jobLocked(jobID)
	if j.msgs[key] == messageID {
		l.mu.Unlock()
		return
	}
	j.msgs[key] = messageID
	// A new message reopens this target's story, so a release recorded for an
	// earlier one (a terminal event before a Retry) must not count this id as
	// already closed — release reads closed against the ids in msgs.
	delete(j.closed, key)
	snapshot := make(map[string]string, len(j.msgs))
	for k, v := range j.msgs {
		// A deleted job's id waiting on its target's drop stays out of the
		// row: the row now is the re-added job's, and the id would outlive
		// the drop there — a restart would edit the deleted job's message.
		if l.dropping[jobID][k] == 0 {
			snapshot[k] = v
		}
	}
	store := l.store
	log := l.log
	l.mu.Unlock()

	if store == nil {
		return
	}
	// OUTSIDE l.mu, but INSIDE writeMu — which is what makes the snapshot
	// above still current when it lands.
	//
	// The lock ORDER is tracker.mu -> db.mu and nothing else: no DB callback
	// re-enters the tracker, so jobLocked's one SELECT per job per boot is
	// held under l.mu without a deadlock to construct. Dropping l.mu here is
	// about the WRITE, which is the call that blocks long enough to matter.
	if !store.UpdateNotificationMsgs(jobID, snapshot) && log != nil {
		log.Debug("notification message id not persisted — job row is gone",
			"jobID", jobID)
	}
}

// forget drops a target's id for a job (a 404 before the re-post). It does NOT
// write: the re-post's remember writes the replacement a moment later, and a
// clearing write in between would double the budget for nothing.
func (l *lifecycleTracker) forget(jobID, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.jobLocked(jobID).msgs, key)
}

// release closes ONE target's story for a job — after a delivered `finished`,
// `error` or `cancelled` edit: its History goes, and the job's whole entry goes
// once no target that holds a message is still open.
//
// Per (job, TARGET), not per job. Two edit-mode targets fan out the same job,
// each on its own FIFO; if the first one's terminal edit dropped the whole
// entry, the second's later terminal edit would render a one-line History
// instead of the story it saw — and, when the id's row write had failed, would
// open a SECOND message for the job instead of editing its own.
//
// The PERSISTED id is untouched, so this is a cache eviction, not a close: a
// Retry (or another target still mid-job) re-reads the row once and keeps
// editing the SAME message, exactly as the owner's ruling requires. Only the
// in-process History restarts, which is already what happens across a restart.
//
// Without it the tracker is a map that only ever grows — one entry per job an
// edit-mode target ever touched, each holding a msgs map and a ~1000-rune
// History per target, for the life of a 24/7 process. A target that filters
// `finished` out keeps the entry alive until evictLocked, which is the same
// bound a job that never reaches a terminal event already has.
//
// keys are the target's key and the old-spelling keys of the second messages
// its terminal edit closed (extraMsg).
func (l *lifecycleTracker) release(jobID string, keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	j := l.jobs[jobID]
	if j == nil {
		return
	}
	if j.closed == nil {
		j.closed = map[string]bool{}
	}
	for _, key := range keys {
		delete(j.history, key)
		j.closed[key] = true
	}
	for k := range j.msgs {
		if !j.closed[k] {
			return
		}
	}
	delete(l.jobs, jobID)
	delete(l.touched, jobID)
}

// dropTarget removes what ONE target holds for a job that no longer exists,
// and the job's entry once nothing is left in it, and clears one of the
// key's marks (markDropping). Unlike release it leaves nothing to re-read:
// the row and the ids it stored are gone. A YouTube job's id is its video
// id, so the same id comes back on a re-add — or when a channel removed and
// re-added re-detects it — and an entry kept from the deleted job used to
// PATCH that job's old message, far up the channel where an edit notifies
// nobody, with the old History carried over.
//
// Per target, and run on that target's sender goroutine behind everything it
// had queued (ForgetJob): see Manager.forgetInOrder for why.
func (l *lifecycleTracker) dropTarget(jobID, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if marks := l.dropping[jobID]; marks[key] > 0 {
		if marks[key]--; marks[key] == 0 {
			delete(marks, key)
		}
		if len(marks) == 0 {
			delete(l.dropping, jobID)
		}
	}
	if j := l.jobs[jobID]; j != nil {
		l.dropKeysLocked(jobID, j, func(k string) bool { return k == key })
	}
}

// retainTarget drops what ONE target holds, as dropTarget does but marking
// nothing off, for every job not in live: the bulk counterpart, for the
// deletes that fire no per-job event (a departed channel's prune). A job
// added after the list was taken can lose its entry, or this target's part
// of it, too; that costs only the in-process History, as a release does —
// its stored ids are re-read on the next touch (dropKeysLocked).
func (l *lifecycleTracker) retainTarget(live map[string]struct{}, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, j := range l.jobs {
		if _, ok := live[id]; !ok {
			l.dropKeysLocked(id, j, func(k string) bool { return k == key })
		}
	}
}

// markDropping marks key for a deleted job (see lifecycleTracker.dropping)
// BEFORE ForgetJob queues the drop that clears the mark — a step that ran
// first would leave a mark nothing clears.
//
// Every key a drop is queued for, not only the keys holding state when the
// delete lands: a target can still have the deleted job's POST queued, or in
// flight, with no id yet. Two edit-mode targets drain at their own pace, so
// the same video id can be re-added, and the quick target post the new job's
// first message, while the slow one still holds — or is yet to record — the
// deleted job's id. That POST writes the job's whole map to the NEW row, and
// without the mark the deleted job's id went with it: the slow target's drop
// clears only memory, so the re-added job edited the deleted job's message
// on that target.
func (l *lifecycleTracker) markDropping(jobID, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	marks := l.dropping[jobID]
	if marks == nil {
		marks = map[string]int{}
		l.dropping[jobID] = marks
	}
	marks[key]++
}

// drop removes, at once, a deleted job's state under every key no queue
// drops in order — the keys not in covered: a target no longer configured,
// whose queue has gone, or one in separate mode.
func (l *lifecycleTracker) drop(jobID string, covered map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if j := l.jobs[jobID]; j != nil {
		l.dropKeysLocked(jobID, j, func(k string) bool { return !covered[k] })
	}
}

// retain is drop for every job not in live: the at-once half of RetainJobs.
func (l *lifecycleTracker) retain(live map[string]struct{}, covered map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, j := range l.jobs {
		if _, ok := live[id]; !ok {
			l.dropKeysLocked(id, j, func(k string) bool { return !covered[k] })
		}
	}
}

// dropKeysLocked removes a job's message id, History and closed mark for
// every key gone admits, then the job's entry once it holds no id and no
// History for anyone. The ForgetJob marks are not the entry's and stay.
// Caller holds l.mu.
//
// An entry that survives with a key dropped is re-read from the row on its
// next touch, like a new one. RetainJobs' list is a snapshot, so the job can
// be live: one target's step dropped its id while a slower target still held
// the entry, nothing re-read the row, and the job's next event on the first
// target opened a second message beside the one the row still names. For a
// deleted job the row is gone, or is a re-added job's, which holds none of
// what the entry still keeps for the deleted one (markDropping).
func (l *lifecycleTracker) dropKeysLocked(jobID string, j *lifecycleJob, gone func(key string) bool) {
	dropped := false
	for k := range j.msgs {
		if gone(k) {
			delete(j.msgs, k)
			dropped = true
		}
	}
	for k := range j.history {
		if gone(k) {
			delete(j.history, k)
			dropped = true
		}
	}
	for k := range j.closed {
		if gone(k) {
			delete(j.closed, k)
		}
	}
	if len(j.msgs) == 0 && len(j.history) == 0 {
		delete(l.jobs, jobID)
		delete(l.touched, jobID)
		return
	}
	if dropped {
		j.loaded = false
	}
}

// hold pins a job's entry for one managed send queued for it (Manager.
// pinLifecycle) until the returned func lets it go, which the queue calls
// once the send has left it — delivered, shed, refused or discarded
// (queued.letGo). Safe to call more than once.
//
// It loads the entry too when it is not in memory: the row still exists when
// the send is queued, and may not when it is dispatched. Deleting an active
// job is cancel, wait, delete, so its "cancelled" is queued when the row
// goes; with its entry evicted — past maxTrackedJobs, by a backfill's
// `found`s on any edit-mode target, before the cancel was queued or while it
// waited behind a busy FIFO — the cancel found neither the id nor the row and
// posted plain, and the message read "Downloading" for good. A ForgetJob step
// cannot keep the entry instead: it is queued once the row has gone, and the
// entry can be gone before that.
//
// The load is a store read on the Send caller's goroutine — a worker, a
// monitor, an HTTP handler — and never a wait on Discord. The send's own
// dispatch would have read the row anyway; this reads it first.
func (l *lifecycleTracker) hold(jobID string) (letGo func()) {
	l.mu.Lock()
	l.held[jobID]++
	l.jobLocked(jobID)
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.held[jobID]--; l.held[jobID] <= 0 {
				delete(l.held, jobID)
			}
		})
	}
}

// heldJobs is how many jobs have a managed send queued. Test-only reader, as
// trackedJobs is: a pin no queue lets go keeps its job's entry for good.
func (l *lifecycleTracker) heldJobs() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}

// trackedJobs is how many jobs the cache is holding. Test-only reader for the
// eviction guarantees; nothing in the program calls it, and that is the point.
func (l *lifecycleTracker) trackedJobs() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.jobs)
}

// evictLocked bounds the map for jobs that never reach a terminal event —
// cancelled outside the notifier, deleted, or filtered down to mid-lifecycle
// keys only. Drops the least-recently-touched entries; each costs one store
// read to rebuild. Caller holds l.mu.
//
// Never keep, the entry the caller is creating, and never a held job's
// (hold): the map can then sit past the cap by the jobs with a send queued,
// which the queues' own cap bounds. keep is the touch's own job — evicting
// it would hand the caller an entry the map no longer holds, and whatever it
// recorded there would be lost.
func (l *lifecycleTracker) evictLocked(keep string) {
	for len(l.jobs) > maxTrackedJobs {
		oldestID, oldest := "", uint64(0)
		for id, seq := range l.touched {
			if id == keep || l.held[id] > 0 {
				continue
			}
			if oldestID == "" || seq < oldest {
				oldestID, oldest = id, seq
			}
		}
		if oldestID == "" {
			return
		}
		delete(l.jobs, oldestID)
		delete(l.touched, oldestID)
	}
}

// appendHistory adds one `<t:unix:R> Label` line and returns the whole field
// value, clamped to historyFieldMax runes by dropping the OLDEST lines. The
// newest state is the one an operator is reading for.
func (l *lifecycleTracker) appendHistory(jobID, key, label string, at time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	j := l.jobLocked(jobID)
	lines := append(j.history[key], fmt.Sprintf("<t:%d:R> %s", at.Unix(), label))
	for len(lines) > 1 && len([]rune(strings.Join(lines, "\n"))) > historyFieldMax {
		lines = lines[1:]
	}
	j.history[key] = lines
	return strings.Join(lines, "\n")
}

// rewriteFields returns Status and History FOLLOWED BY the event's own fields.
//
// The order is load-bearing, not cosmetic. N1's clampEmbed enforces Discord's
// 6000-character embed total by dropping TRAILING fields first, so a fat
// producer (a mux error, a long format-selection line, a dozen segment stats)
// would throw the History away and then the Status — leaving a lifecycle
// message with no state line at all, which is the one thing it exists to show.
// First position makes them the last two fields a clamp could ever reach.
//
// The caller's slice is never aliased — producers reuse their FieldBuilder.
func (l *lifecycleTracker) rewriteFields(jobID, key, event string, fields []Field, at time.Time) []Field {
	label := lifecycleLabels[event]
	if label == "" {
		label = event
	}
	out := make([]Field, 0, len(fields)+2)
	out = append(out, Field{Name: statusFieldName, Value: label, Inline: true})
	out = append(out, Field{Name: historyFieldName, Value: l.appendHistory(jobID, key, label, at)})
	out = append(out, fields...)
	return out
}

// lifecyclePlan is the ONE decision: post separately, create the job's message,
// or edit it — and, for a terminal event, do both.
type lifecyclePlan struct {
	// Manage is false for every ordinary send; the sender then behaves exactly
	// as it did before this arc existed.
	Manage bool
	// MessageID is the message to PATCH. Empty means "create it".
	MessageID string
	// Extra are the job's other messages on this target, which every edit
	// rewrites with the same body (messageID).
	Extra []extraMsg
	// AlsoSeparate marks a terminal event: edit the lifecycle message, then
	// post the separate embed too (it carries the mention).
	AlsoSeparate bool
}

// managesLifecycle reports whether a send of opts on t is one an edit-mode
// target creates, edits or closes a job's lifecycle message with — the cheap
// half of planLifecycle and pinLifecycle, which no tracker call precedes.
func managesLifecycle(t notificationTarget, opts SendOptions) bool {
	if t.mode != ModeEdit || t.msgKey == "" || opts.JobID == "" || opts.Event == "" {
		return false
	}
	return lifecycleEvents[opts.Event] || terminalLifecycleEvents[opts.Event]
}

// pinLifecycle pins the job's tracker entry (lifecycleTracker.hold) for one
// message queued on t that dispatchOne will manage — a job's single-embed
// lifecycle or terminal event, on an edit-mode target whose transport can
// edit — and returns what lets it go. nil for any other message: dispatchOne
// never reads the entry for one.
func (m *Manager) pinLifecycle(t notificationTarget, msg Message) (letGo func()) {
	if len(msg.Embeds) != 1 {
		return nil
	}
	if _, editable := t.sender.(editableSender); !editable {
		return nil
	}
	if opts := msg.Embeds[0].Opts; managesLifecycle(t, opts) {
		return m.tracker().hold(opts.JobID)
	}
	return nil
}

// planLifecycle answers the POST-or-PATCH question for one queued send.
func (m *Manager) planLifecycle(t notificationTarget, opts SendOptions) lifecyclePlan {
	// The cheap checks — two map lookups among them — come BEFORE any tracker
	// call: messageID creates the job's entry and does its one store read, so
	// asking it about an event that can never manage a message would spend a
	// SELECT and a tracker slot on nothing.
	if !managesLifecycle(t, opts) {
		return lifecyclePlan{}
	}
	if terminalLifecycleEvents[opts.Event] {
		// Close an OPEN message; never open one. A job whose first word to
		// this target is "failed" has no story to rewrite.
		if id, extra := m.tracker().messageID(opts.JobID, t.msgKey, t.legacyMsgKeys...); id != "" {
			return lifecyclePlan{Manage: true, MessageID: id, Extra: extra, AlsoSeparate: true}
		}
		// The lookup just created the entry, and this event is the end of the
		// story: release it again rather than leave a slot (and the store read
		// behind it) held for a message that was never opened. On a target
		// filtered to ["error","finished"] that is every failing job.
		m.tracker().release(opts.JobID, t.msgKey)
		return lifecyclePlan{}
	}
	id, extra := m.tracker().messageID(opts.JobID, t.msgKey, t.legacyMsgKeys...)
	return lifecyclePlan{Manage: true, MessageID: id, Extra: extra}
}

// dispatchOne is the single decision point between the per-target FIFO sender
// and the wire: post as usual, create the job's lifecycle message, or edit it.
//
// Named dispatchOne, not deliver: after the merge there is already a
// (*targetQueue).deliver above it and a (*DiscordWebhook).deliver below it, and
// a three-deep `q.deliver -> m.deliver -> d.deliver` chain is a reading trap.
//
// It runs ON the per-target sender goroutine, so a job's states can never
// reorder and a PATCH can never overtake the POST that created its message.
// once is the queue's shutting-down flag: when set, every request on this path
// is single-attempt, because the owner's 15 s force-exit cap must not be spent
// on one lifecycle edit's retry ladder.
func (m *Manager) dispatchOne(t notificationTarget, msg Message, once bool) error {
	// A batched message is several jobs' embeds in one POST and by ruling
	// never belongs to an edit-mode target; guard anyway, because a body with
	// ten embeds cannot be one job's lifecycle message.
	if len(msg.Embeds) != 1 {
		return sendPlain(t.sender, msg, once)
	}
	opts := msg.Embeds[0].Opts
	// The editability question comes FIRST: a transport that cannot edit falls
	// back to a plain post rather than dropping the event, and planLifecycle's
	// messageID would otherwise spend a store read and hold a tracker entry per
	// job for messages this transport can never rewrite.
	edit, editable := t.sender.(editableSender)
	if !editable {
		if opts.EditOnly {
			return nil // nothing to close here, and the report is suppressed
		}
		return sendPlain(t.sender, msg, once)
	}
	plan := m.planLifecycle(t, opts)
	if opts.EditOnly && plan.MessageID == "" {
		// No open message to close (a terminal event never opens one), or
		// not an edit-mode target at all.
		return nil
	}
	if !plan.Manage {
		return sendPlain(t.sender, msg, once)
	}

	tr := m.tracker()
	e := msg.Embeds[0]
	e.Fields = tr.rewriteFields(e.Opts.JobID, t.msgKey, e.Opts.Event, e.Fields, e.eventTime())
	// The ping is per MESSAGE (content + allowed_mentions), so it is carried
	// over from the queued Message, not rebuilt from the embed. An EditOnly
	// close carries none: its report is suppressed, and the role text would
	// still show in the edited message.
	mention, allowed := msg.Mention, msg.MentionAllowed
	if opts.EditOnly {
		mention, allowed = "", nil
	}
	body, err := buildPayload(Message{Embeds: []Embed{e}, Mention: mention, MentionAllowed: allowed})
	if err != nil {
		return err
	}

	lifecycleErr := m.postOrPatch(edit, tr, opts.JobID, t.msgKey, opts.Event, plan, body, once)
	closedExtra, extraErr := m.patchExtra(edit, tr, opts.JobID, plan.Extra, body, once)

	if plan.AlsoSeparate && !opts.EditOnly {
		// The separate embed carries the mention and must go out even if the
		// closing edit failed (owner ruling: two messages on failure).
		if sepErr := sendPlain(t.sender, msg, once); sepErr != nil {
			return errors.Join(lifecycleErr, extraErr, sepErr)
		}
	}
	// A delivered terminal edit ends this job's story for THIS target: drop its
	// history (and the job's entry once every target holding a message is
	// closed). The PERSISTED id stays, so a Retry reloads it once and keeps
	// editing the same message.
	if lifecycleErr == nil && (plan.AlsoSeparate || opts.Event == "finished") {
		tr.release(opts.JobID, append([]string{t.msgKey}, closedExtra...)...)
	}
	return errors.Join(lifecycleErr, extraErr)
}

// patchExtra rewrites the job's other messages on this target (lifecyclePlan.
// Extra) with the body its own message got, and returns the keys of those it
// delivered. One that Discord answers "Unknown Message" is forgotten — there
// is nothing left of it to close — and nothing is posted in its place: the
// job's own message carries the story.
func (m *Manager) patchExtra(edit editableSender, tr *lifecycleTracker, jobID string, extra []extraMsg, body []byte, once bool) (delivered []string, err error) {
	for _, x := range extra {
		var perr error
		if once {
			perr = edit.patchMessageOnce(x.id, body)
		} else {
			perr = edit.patchMessage(x.id, body)
		}
		switch {
		case perr == nil:
			delivered = append(delivered, x.key)
		case errors.Is(perr, ErrUnknownMessage):
			tr.forget(jobID, x.key)
			m.logger.Info("an old spelling's lifecycle message is gone — no longer editing it", "jobID", jobID)
		default:
			err = errors.Join(err, perr)
		}
	}
	return delivered, err
}

// sendPlain posts a message unchanged, honouring the queue's shutting-down
// flag exactly as (*targetQueue).deliver did before this arc existed.
func sendPlain(s sender, msg Message, once bool) error {
	if once {
		return s.SendOnce(msg)
	}
	return s.Send(msg)
}

// postOrPatch performs the wire half of dispatchOne, including the one
// recovery this path has: a PATCH that Discord answers "Unknown Message"
// becomes a fresh POST whose id overwrites the stored one.
func (m *Manager) postOrPatch(edit editableSender, tr *lifecycleTracker, jobID, msgKey, event string, plan lifecyclePlan, body []byte, once bool) error {
	if plan.MessageID != "" {
		var err error
		if once {
			err = edit.patchMessageOnce(plan.MessageID, body)
		} else {
			err = edit.patchMessage(plan.MessageID, body)
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrUnknownMessage) {
			return err
		}
		tr.forget(jobID, msgKey)
		if plan.AlsoSeparate {
			// A terminal event never CREATES a lifecycle message
			// (terminalLifecycleEvents): its separate embed, which dispatchOne
			// posts next, is the whole report. Re-posting here sent two
			// messages for one failure and stored a terminal-look message as
			// the one a later Retry would go on editing.
			m.logger.Info("lifecycle message is gone — the terminal event's own post stands in for it",
				"jobID", jobID, "event", event)
			return nil
		}
		m.logger.Info("lifecycle message is gone — posting a new one",
			"jobID", jobID, "event", event)
		// fall through to the create path
	}
	var (
		id  string
		err error
	)
	if once {
		id, err = edit.postWaitOnce(body)
	} else {
		id, err = edit.postWait(body)
	}
	if err != nil {
		// A re-POST that itself fails leaves the id already forgotten, so the
		// job's next event creates a message rather than PATCHing a ghost.
		return err
	}
	tr.remember(jobID, msgKey, id)
	return nil
}

// editableSender is the optional capability a transport advertises when it can
// create a message it is able to rewrite later. Only *DiscordWebhook
// implements it; anything else falls back to separate posts.
type editableSender interface {
	postWait(body []byte) (string, error)
	patchMessage(messageID string, body []byte) error
	postWaitOnce(body []byte) (string, error)
	patchMessageOnce(messageID string, body []byte) error
}

// tracker returns the lifecycle tracker, creating it on first use.
func (m *Manager) tracker() *lifecycleTracker {
	m.trackerOnce.Do(func() {
		if m.lifecycle == nil {
			m.lifecycle = newLifecycleTracker(nil)
		}
		m.lifecycle.log = m.logger
	})
	return m.lifecycle
}

// SetMessageStore attaches the persistence backend for edit-mode message ids.
// Wired once at boot (cmd/moombox) with the live *database.Database. Without
// it edit mode still works within a process; only restart survival is lost.
func (m *Manager) SetMessageStore(s MessageStore) {
	m.tracker().setStore(s)
}

// ForgetJob drops the edit-mode state held for a deleted job (see dropTarget).
// cmd/moombox calls it from its OnJobDeleted subscriber.
func (m *Manager) ForgetJob(jobID string) {
	if m == nil || jobID == "" {
		return
	}
	tr := m.tracker()
	covered := m.forgetInOrder(
		func(key string) { tr.markDropping(jobID, key) },
		func(key string) { tr.dropTarget(jobID, key) })
	tr.drop(jobID, covered)
}

// RetainJobs drops the edit-mode state of every job not in live (see
// retainTarget). cmd/moombox calls it from its OnJobsChange subscriber, which
// is the only event a bulk delete fires.
//
// No markDropping here. live is a list taken at the bulk write, so a job added
// since is missing from it without being deleted, and marking it would keep
// its first message id out of its own row — its next event after the drop
// would open a second message.
func (m *Manager) RetainJobs(live map[string]struct{}) {
	if m == nil {
		return
	}
	// Copied: the steps read it later, on every target's sender goroutine.
	kept := make(map[string]struct{}, len(live))
	for id := range live {
		kept[id] = struct{}{}
	}
	tr := m.tracker()
	covered := m.forgetInOrder(nil, func(key string) { tr.retainTarget(kept, key) })
	tr.retain(kept, covered)
}

// forgetInOrder queues step(key) on the FIFO of every target that can hold
// edit-mode state (editKeys) — the live ones and any retired one still
// finishing its delivery in flight — and returns the keys it covered. mark,
// when set, runs for each key just before its step is queued. A queue whose
// goroutine has returned runs nothing again, so its step runs at once. The
// caller drops every other key at once.
//
// In order, not at once, because a deleted job can still have deliveries on
// its way. Deleting an active job cancels it first, so its "cancelled" is
// already queued when the delete lands; if the target was busy — another
// job's request in flight, a rate-limit wait — that send was dispatched after
// the drop, found neither the in-memory id nor the row, and posted plain: the
// lifecycle message read "Downloading" for good. And a POST in flight when the
// drop landed came back and remembered its id afresh, so a re-add of the same
// video id PATCHed the deleted job's message — the very thing the drop is
// for. Behind everything queued, the drop runs after both, and before any
// send of a job re-added under the same id.
func (m *Manager) forgetInOrder(mark, step func(key string)) map[string]bool {
	m.targetsMu.RLock()
	queues := make([]*targetQueue, 0, len(m.targets)+len(m.retiring))
	queues = append(queues, m.targets...)
	for _, q := range m.retiring {
		queues = append(queues, q)
	}
	m.targetsMu.RUnlock()

	covered := make(map[string]bool, len(queues))
	for _, q := range queues {
		for _, key := range q.editKeys() {
			if mark != nil {
				mark(key)
			}
			if !q.enqueueControl(func() { step(key) }) {
				step(key)
			}
			covered[key] = true
		}
	}
	return covered
}
