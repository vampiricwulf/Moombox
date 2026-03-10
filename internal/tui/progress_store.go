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
}

// NewProgressStore creates a new progress store.
func NewProgressStore() *ProgressStore {
	return &ProgressStore{
		items: make(map[string]*ProgressData),
	}
}

// Set updates progress data for a job.
func (s *ProgressStore) Set(jobID string, data *ProgressData) {
	s.items[jobID] = data
}

// Get returns progress data for a job, or nil.
func (s *ProgressStore) Get(jobID string) *ProgressData {
	return s.items[jobID]
}

// Delete removes progress data for a job.
func (s *ProgressStore) Delete(jobID string) {
	delete(s.items, jobID)
}

// Clear removes all entries.
func (s *ProgressStore) Clear() {
	s.items = make(map[string]*ProgressData)
}
