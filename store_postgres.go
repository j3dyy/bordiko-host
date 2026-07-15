package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the durable, event-sourced Store used when DATABASE_URL is
// set. `matches` holds the latest snapshot; `moves` is the append-only log.
type PostgresStore struct {
	pool *pgxpool.Pool
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS matches (
    id         text PRIMARY KEY,
    game_id    text NOT NULL,
    seed       text NOT NULL,
    players    jsonb NOT NULL,
    state      jsonb NOT NULL,
    move_count int NOT NULL,
    ended      boolean NOT NULL DEFAULT false,
    result     jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS moves (
    match_id   text NOT NULL REFERENCES matches(id) ON DELETE CASCADE,
    idx        int NOT NULL,
    move       jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (match_id, idx)
);
-- The published build a match is pinned to. Added after matches already existed,
-- hence the additive ALTER: pre-existing rows keep '' and fall back to resolving
-- whatever build is available, exactly as they did before pinning.
ALTER TABLE matches ADD COLUMN IF NOT EXISTS game_version text NOT NULL DEFAULT '';
`

func NewPostgresStore(ctx context.Context, url string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) CreateMatch(ctx context.Context, m *Match) error {
	players, _ := json.Marshal(m.Players)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO matches (id, game_id, game_version, seed, players, state, move_count, ended, result)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		m.ID, m.GameID, m.GameVersion, m.Seed, players, []byte(m.State), m.MoveCount, m.Ended, nullableJSON(m.Result))
	return err
}

func (s *PostgresStore) GetMatch(ctx context.Context, id string) (*Match, error) {
	var (
		m       Match
		players []byte
		state   []byte
		result  []byte
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, game_id, game_version, seed, players, state, move_count, ended, result, created_at
		 FROM matches WHERE id = $1`, id).
		Scan(&m.ID, &m.GameID, &m.GameVersion, &m.Seed, &players, &state, &m.MoveCount, &m.Ended, &result, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMatchNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(players, &m.Players)
	m.State = state
	m.Result = result
	return &m, nil
}

func (s *PostgresStore) CountsByGame(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT game_id, count(*) FROM matches GROUP BY game_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			g string
			c int
		)
		if err := rows.Scan(&g, &c); err == nil {
			out[g] = c
		}
	}
	return out, nil
}

func (s *PostgresStore) ActiveMatchForPlayer(ctx context.Context, playerID string) (string, string, bool, error) {
	needle, _ := json.Marshal([]string{playerID})
	var id, gameID string
	err := s.pool.QueryRow(ctx,
		`SELECT id, game_id FROM matches
		 WHERE ended = false AND players @> $1::jsonb
		 ORDER BY created_at DESC LIMIT 1`, string(needle)).Scan(&id, &gameID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return id, gameID, true, nil
}

func (s *PostgresStore) AppendMove(ctx context.Context, id string, move, newState, result json.RawMessage, moveCount int, ended bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// idx is 0-based; the move being appended is number (moveCount-1).
	if _, err := tx.Exec(ctx,
		`INSERT INTO moves (match_id, idx, move) VALUES ($1,$2,$3)`,
		id, moveCount-1, []byte(move)); err != nil {
		return fmt.Errorf("insert move: %w", err)
	}
	ct, err := tx.Exec(ctx,
		`UPDATE matches SET state=$2, move_count=$3, ended=$4, result=$5 WHERE id=$1`,
		id, []byte(newState), moveCount, ended, nullableJSON(result))
	if err != nil {
		return fmt.Errorf("update match: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrMatchNotFound
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) Close() error {
	s.pool.Close()
	return nil
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return []byte(b)
}
