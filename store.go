package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrMatchNotFound = errors.New("match not found")

// Match is the authoritative, event-sourced record of a game in progress.
// State is the engine's canonical MatchState (which itself embeds the move log);
// the Store additionally keeps an append-only moves log for auditing/replay.
type Match struct {
	ID        string          `json:"id"`
	GameID    string          `json:"gameId"`
	Seed      string          `json:"seed"`
	Players   []string        `json:"players"`
	State     json.RawMessage `json:"-"`
	MoveCount int             `json:"moveCount"`
	Ended     bool            `json:"ended"`
	Result    json.RawMessage `json:"result,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Store persists matches and their move log. Two implementations exist: an
// in-memory store (default, for dev/tests) and a Postgres store (when
// DATABASE_URL is set). Both are event-sourced: AppendMove records the move and
// the resulting state atomically.
type Store interface {
	CreateMatch(ctx context.Context, m *Match) error
	GetMatch(ctx context.Context, id string) (*Match, error)
	AppendMove(ctx context.Context, id string, move, newState, result json.RawMessage, moveCount int, ended bool) error
	Close() error
}
