package api

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// taskStore is a simple in-memory task store.
type taskStore struct {
	mu    sync.RWMutex
	tasks map[string]*Task // keyed by public task ID
}

var globalTaskStore = &taskStore{
	tasks: make(map[string]*Task),
}

// generateTaskID creates a unique public task ID in the format "task_xxxx".
func generateTaskID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "task_" + hex.EncodeToString([]byte(time.Now().String()))
	}
	return "task_" + hex.EncodeToString(b)
}

// Insert stores a new task.
func (s *taskStore) Insert(task *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = task
}

// Get retrieves a task by its public ID.
func (s *taskStore) Get(taskID string) *Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[taskID]
}

// GetUnfinished returns all tasks that are not in a terminal state.
func (s *taskStore) GetUnfinished() []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Task
	for _, t := range s.tasks {
		if t.Status != TaskStatusSuccess && t.Status != TaskStatusFailure {
			result = append(result, t)
		}
	}
	return result
}

// Update modifies a task in place. Caller should hold no locks.
func (s *taskStore) Update(taskID string, fn func(t *Task)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[taskID]; ok {
		fn(t)
	}
}

// Cleanup removes tasks older than maxAge.
func (s *taskStore) Cleanup(maxAge time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for id, t := range s.tasks {
		if t.CreatedAt.Before(cutoff) {
			delete(s.tasks, id)
		}
	}
}
