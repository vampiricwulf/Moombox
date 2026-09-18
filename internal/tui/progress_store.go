package tui

// ProgressData stores high-frequency download progress without triggering
// TUI re-sorts. Only status changes cause re-renders; progress reads are
// zero-cost lookups from this store during View().
type ProgressData struct {
	Progress          string
	Percent           float64
	Speed             string
	ETA               string
	LastVideoSeq      *int
	LastAudioSeq      *int
	TotalVideoSeq     *int
	TotalAudioSeq     *int
	TotalChatMessages *int
	ChatStatus        string
}

// ProgressStore is a map of job ID -> progress data.
// All access occurs on the main BubbleTea goroutine (Update/View),
// so no synchronization is needed.
type ProgressStore struct {
	items map[string]*ProgressData
	// rev increments on every write. The task list renders each active row's
	// percent straight out of this store, and a progress-only job update
	// never touches the task list model at all (app_update.go gates
	// UpdateJob on hasDisplayChange) — so the row's render cache keys on
	// this counter rather than on anything the model can observe (CORE-2).
	rev uint64
}

// Rev returns the store's write revision. Equal revisions mean no entry has
// been Set, Deleted or Cleared since.
func (s *ProgressStore) Rev() uint64 { return s.rev }

// NewProgressStore creates a new progress store.
func NewProgressStore() *ProgressStore {
	return &ProgressStore{
		items: make(map[string]*ProgressData),
	}
}

// Set updates progress data for a job.
func (s *ProgressStore) Set(jobID string, data *ProgressData) {
	s.items[jobID] = data
	s.rev++
}

// Get returns progress data for a job, or nil.
func (s *ProgressStore) Get(jobID string) *ProgressData {
	return s.items[jobID]
}

// Delete removes progress data for a job.
func (s *ProgressStore) Delete(jobID string) {
	delete(s.items, jobID)
	s.rev++
}

// Clear removes all entries.
func (s *ProgressStore) Clear() {
	s.items = make(map[string]*ProgressData)
	s.rev++
}
