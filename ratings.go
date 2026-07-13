package main

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ratings are the per-game competitive ladder. When a match ends, the winner and
// loser(s) have their record updated; for head-to-head (exactly two players) the
// rating moves by ELO. Ratings are keyed by the player string, which — because
// login is required to play — is a stable Bordiko user id, so the gateway can
// resolve display names for the leaderboard.

const (
	eloK       = 32.0
	baseRating = 1200.0
)

// RatingEntry is one player's standing in a game.
type RatingEntry struct {
	Player string  `json:"player"`
	Rating float64 `json:"rating"`
	Wins   int     `json:"wins"`
	Losses int     `json:"losses"`
	Draws  int     `json:"draws"`
	Games  int     `json:"games"`
}

// matchResult is the shape the engine emits for a finished match. A game may end
// with a single `winner`, a partnership `winners` list (e.g. Jokeri teams), an
// explicit `losers` list (e.g. a forfeit — the leaver's team loses, everyone
// else wins), or a `draw`.
type matchResult struct {
	Winner  string   `json:"winner"`
	Winners []string `json:"winners"`
	Losers  []string `json:"losers"`
	Draw    bool     `json:"draw"`
}

func parseResult(raw json.RawMessage) (matchResult, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return matchResult{}, false
	}
	var r matchResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return matchResult{}, false
	}
	return r, true
}

// RatingsStore records match outcomes and serves leaderboards. Two
// implementations: in-memory (default) and Postgres (when DATABASE_URL is set).
type RatingsStore interface {
	RecordResult(ctx context.Context, gameID string, players []string, result matchResult) error
	Leaderboard(ctx context.Context, gameID string, limit int) ([]RatingEntry, error)
	Close() error
}

// elo returns the two new ratings after a head-to-head game. scoreA is A's score
// (1 win, 0.5 draw, 0 loss); scoreB = 1 - scoreA.
func elo(ra, rb, scoreA float64) (float64, float64) {
	ea := 1.0 / (1.0 + math.Pow(10, (rb-ra)/400.0))
	eb := 1.0 / (1.0 + math.Pow(10, (ra-rb)/400.0))
	return ra + eloK*(scoreA-ea), rb + eloK*((1-scoreA)-eb)
}

// outcome classifies each player's result: +1 win, 0 draw, -1 loss. Team results
// (winners/losers lists) take precedence over the single-winner form.
func outcomes(players []string, r matchResult) map[string]int {
	out := make(map[string]int, len(players))
	win := make(map[string]bool, len(r.Winners))
	lose := make(map[string]bool, len(r.Losers))
	for _, p := range r.Winners {
		win[p] = true
	}
	for _, p := range r.Losers {
		lose[p] = true
	}
	for _, p := range players {
		switch {
		case r.Draw:
			out[p] = 0
		case len(r.Winners) > 0:
			if win[p] {
				out[p] = 1
			} else {
				out[p] = -1
			}
		case len(r.Losers) > 0:
			if lose[p] {
				out[p] = -1
			} else {
				out[p] = 1
			}
		case r.Winner != "" && p == r.Winner:
			out[p] = 1
		case r.Winner != "":
			out[p] = -1
		}
	}
	return out
}

/* ------------------------------- in-memory -------------------------------- */

type memRating struct {
	rating              float64
	wins, losses, draws int
	games               int
}

type MemoryRatings struct {
	mu     sync.Mutex
	byGame map[string]map[string]*memRating
}

func NewMemoryRatings() *MemoryRatings {
	return &MemoryRatings{byGame: map[string]map[string]*memRating{}}
}

func (s *MemoryRatings) get(gameID, player string) *memRating {
	g := s.byGame[gameID]
	if g == nil {
		g = map[string]*memRating{}
		s.byGame[gameID] = g
	}
	r := g[player]
	if r == nil {
		r = &memRating{rating: baseRating}
		g[player] = r
	}
	return r
}

func (s *MemoryRatings) RecordResult(_ context.Context, gameID string, players []string, result matchResult) error {
	if _, ok := parseValid(result); !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oc := outcomes(players, result)
	for _, p := range players {
		r := s.get(gameID, p)
		r.games++
		switch oc[p] {
		case 1:
			r.wins++
		case -1:
			r.losses++
		default:
			r.draws++
		}
	}
	if len(players) == 2 {
		a, b := s.get(gameID, players[0]), s.get(gameID, players[1])
		scoreA := scoreFromOutcome(oc[players[0]])
		a.rating, b.rating = elo(a.rating, b.rating, scoreA)
	}
	return nil
}

func (s *MemoryRatings) Leaderboard(_ context.Context, gameID string, limit int) ([]RatingEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []RatingEntry{}
	collect := func(gid string, m map[string]*memRating) {
		for p, r := range m {
			_ = gid
			out = append(out, RatingEntry{Player: p, Rating: r.rating, Wins: r.wins, Losses: r.losses, Draws: r.draws, Games: r.games})
		}
	}
	if gameID != "" {
		collect(gameID, s.byGame[gameID])
	} else {
		for gid, m := range s.byGame {
			collect(gid, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rating > out[j].Rating })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryRatings) Close() error { return nil }

/* -------------------------------- postgres -------------------------------- */

type PostgresRatings struct {
	pool *pgxpool.Pool
}

const ratingsSchemaSQL = `
CREATE TABLE IF NOT EXISTS ratings (
    game_id    text NOT NULL,
    player     text NOT NULL,
    rating     double precision NOT NULL DEFAULT 1200,
    wins       int NOT NULL DEFAULT 0,
    losses     int NOT NULL DEFAULT 0,
    draws      int NOT NULL DEFAULT 0,
    games      int NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (game_id, player)
);`

func NewPostgresRatings(ctx context.Context, pool *pgxpool.Pool) (*PostgresRatings, error) {
	if _, err := pool.Exec(ctx, ratingsSchemaSQL); err != nil {
		return nil, err
	}
	return &PostgresRatings{pool: pool}, nil
}

func (s *PostgresRatings) RecordResult(ctx context.Context, gameID string, players []string, result matchResult) error {
	if _, ok := parseValid(result); !ok {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Ensure a row exists for every player, then lock them for the update.
	for _, p := range players {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ratings (game_id, player) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			gameID, p); err != nil {
			return err
		}
	}
	oc := outcomes(players, result)
	for _, p := range players {
		var w, l, d string
		switch oc[p] {
		case 1:
			w, l, d = "1", "0", "0"
		case -1:
			w, l, d = "0", "1", "0"
		default:
			w, l, d = "0", "0", "1"
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ratings
			   SET wins = wins + `+w+`, losses = losses + `+l+`, draws = draws + `+d+`,
			       games = games + 1, updated_at = now()
			 WHERE game_id = $1 AND player = $2`, gameID, p); err != nil {
			return err
		}
	}
	if len(players) == 2 {
		ra, err := lockRating(ctx, tx, gameID, players[0])
		if err != nil {
			return err
		}
		rb, err := lockRating(ctx, tx, gameID, players[1])
		if err != nil {
			return err
		}
		na, nb := elo(ra, rb, scoreFromOutcome(oc[players[0]]))
		if _, err := tx.Exec(ctx, `UPDATE ratings SET rating=$3 WHERE game_id=$1 AND player=$2`, gameID, players[0], na); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ratings SET rating=$3 WHERE game_id=$1 AND player=$2`, gameID, players[1], nb); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func lockRating(ctx context.Context, tx pgx.Tx, gameID, player string) (float64, error) {
	var r float64
	err := tx.QueryRow(ctx, `SELECT rating FROM ratings WHERE game_id=$1 AND player=$2 FOR UPDATE`, gameID, player).Scan(&r)
	return r, err
}

func (s *PostgresRatings) Leaderboard(ctx context.Context, gameID string, limit int) ([]RatingEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT player, rating, wins, losses, draws, games FROM ratings WHERE game_id=$1 ORDER BY rating DESC LIMIT $2`
	args := []any{gameID, limit}
	if gameID == "" {
		query = `SELECT player, rating, wins, losses, draws, games FROM ratings ORDER BY rating DESC LIMIT $1`
		args = []any{limit}
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RatingEntry{}
	for rows.Next() {
		var e RatingEntry
		if err := rows.Scan(&e.Player, &e.Rating, &e.Wins, &e.Losses, &e.Draws, &e.Games); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PostgresRatings) Close() error { return nil } // pool is owned by the match store

/* -------------------------------- helpers --------------------------------- */

func scoreFromOutcome(o int) float64 {
	switch o {
	case 1:
		return 1.0
	case -1:
		return 0.0
	default:
		return 0.5
	}
}

// parseValid reports whether a result actually decides the game (a winner, a
// team result, or a draw); an empty/garbage result is ignored so ratings aren't
// touched.
func parseValid(r matchResult) (matchResult, bool) {
	return r, r.Draw || r.Winner != "" || len(r.Winners) > 0 || len(r.Losers) > 0
}
