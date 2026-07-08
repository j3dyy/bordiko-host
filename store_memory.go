package main

import (
	"context"
	"encoding/json"
	"sync"
)

// MemoryStore is an in-process Store for development and tests.
type MemoryStore struct {
	mu      sync.RWMutex
	matches map[string]*Match
	moves   map[string][]json.RawMessage
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		matches: make(map[string]*Match),
		moves:   make(map[string][]json.RawMessage),
	}
}

func (s *MemoryStore) CreateMatch(_ context.Context, m *Match) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *m
	s.matches[m.ID] = &cp
	s.moves[m.ID] = nil
	return nil
}

func (s *MemoryStore) GetMatch(_ context.Context, id string) (*Match, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.matches[id]
	if !ok {
		return nil, ErrMatchNotFound
	}
	cp := *m
	return &cp, nil
}

func (s *MemoryStore) AppendMove(_ context.Context, id string, move, newState, result json.RawMessage, moveCount int, ended bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.matches[id]
	if !ok {
		return ErrMatchNotFound
	}
	s.moves[id] = append(s.moves[id], move)
	m.State = newState
	m.MoveCount = moveCount
	m.Ended = ended
	m.Result = result
	return nil
}

func (s *MemoryStore) Close() error { return nil }
