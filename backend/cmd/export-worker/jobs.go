package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// JobStatus values
const (
	JobStatusRunning = "running"
	JobStatusDone    = "done"
	JobStatusFailed  = "failed"
)

// Job holds the state of a single export job.
type Job struct {
	ID        string
	Status    string
	Err       string
	ResultKey string
	Created   time.Time
}

// JobManager is an in-memory store for export jobs with a background sweeper.
type JobManager struct {
	mu   sync.RWMutex
	jobs map[string]*Job

	sweepTTL time.Duration
}

// NewJobManager creates a JobManager and starts the background sweeper.
func NewJobManager(sweepTTL time.Duration) *JobManager {
	m := &JobManager{
		jobs:     make(map[string]*Job),
		sweepTTL: sweepTTL,
	}
	go m.sweep()
	return m
}

// Create allocates a new running job and returns its ID.
func (m *JobManager) Create() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)

	m.mu.Lock()
	m.jobs[id] = &Job{
		ID:      id,
		Status:  JobStatusRunning,
		Created: time.Now(),
	}
	m.mu.Unlock()
	return id, nil
}

// Get retrieves a job by ID. Returns nil if not found.
func (m *JobManager) Get(id string) *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil
	}
	// Return a copy so callers cannot mutate the internal state.
	cp := *j
	return &cp
}

// MarkDone transitions a job to done and records the result S3 key.
func (m *JobManager) MarkDone(id, resultKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.Status = JobStatusDone
		j.ResultKey = resultKey
	}
}

// MarkFailed transitions a job to failed and records the error message.
func (m *JobManager) MarkFailed(id, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.Status = JobStatusFailed
		j.Err = errMsg
	}
}

// sweep runs in the background and removes jobs older than sweepTTL.
func (m *JobManager) sweep() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-m.sweepTTL)
		m.mu.Lock()
		for id, j := range m.jobs {
			if j.Created.Before(cutoff) {
				delete(m.jobs, id)
			}
		}
		m.mu.Unlock()
	}
}
