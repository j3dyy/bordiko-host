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
	ID     string `json:"id"`
	GameID string `json:"gameId"`
	// GameVersion pins the published build this match was created with. Every
	// move is replayed against the same reducer, so publishing an update never
	// changes a match already in flight. Empty for matches created before
	// pinning existed (and for local GAMES_DIR builds).
	GameVersion string          `json:"gameVersion,omitempty"`
	Seed        string          `json:"seed"`
	Players     []string        `json:"players"`
	State       json.RawMessage `json:"-"`
	MoveCount   int             `json:"moveCount"`
	Ended       bool            `json:"ended"`
	Result      json.RawMessage `json:"result,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
}

// Store persists matches and their move log. Two implementations exist: an
// in-memory store (default, for dev/tests) and a Postgres store (when
// DATABASE_URL is set). Both are event-sourced: AppendMove records the move and
// the resulting state atomically.
type Store interface {
	CreateMatch(ctx context.Context, m *Match) error
	GetMatch(ctx context.Context, id string) (*Match, error)
	AppendMove(ctx context.Context, id string, move, newState, result json.RawMessage, moveCount int, ended bool) error
	// CountsByGame returns how many matches exist per game id (the "plays"
	// metric the marketplace catalog surfaces).
	CountsByGame(ctx context.Context) (map[string]int, error)
	// ActiveMatchForPlayer returns the id + game of an unfinished match the
	// player is in, if any (used to enforce "one game at a time" + resume).
	ActiveMatchForPlayer(ctx context.Context, playerID string) (id, gameID string, ok bool, err error)
	// DeleteMatch removes a match and its moves. Used when a real-time match
	// migrates to the in-memory hot store — its durable copy is deleted so it
	// doesn't linger as an "active" orphan that traps the player.
	DeleteMatch(ctx context.Context, id string) error
	Close() error
}
