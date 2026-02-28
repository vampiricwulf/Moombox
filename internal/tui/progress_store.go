package tui

import "sync"

// ProgressData stores high-frequency download progress without triggering
// TUI re-sorts. Only status changes cause re-renders; progress reads are
// zero-cost lookups from this store during View().
type ProgressData struct {
	Progress         string
	Percent          float64
	Speed            string
	ETA              string
	LastVideoSeq     *int
	LastAudioSeq     *int
	TotalVideoSeq    *int
	TotalAudioSeq    *int
	TotalChatMessages *int
	ChatStatus       string
}

// ProgressStore is a thread-safe map of job ID -> progress data.
// It avoids re-sorting the task list on every progress tick (100ms).
type ProgressStore struct {
	mu    sync.RWMutex
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
	s.mu.Lock()
	s.items[jobID] = data
	s.mu.Unlock()
}

// Get returns progress data for a job, or nil.
func (s *ProgressStore) Get(jobID string) *ProgressData {
	s.mu.RLock()
	d := s.items[jobID]
	s.mu.RUnlock()
	return d
}

// Delete removes progress data for a job.
func (s *ProgressStore) Delete(jobID string) {
	s.mu.Lock()
	delete(s.items, jobID)
	s.mu.Unlock()
}

// Clear removes all entries.
func (s *ProgressStore) Clear() {
	s.mu.Lock()
	s.items = make(map[string]*ProgressData)
	s.mu.Unlock()
}
