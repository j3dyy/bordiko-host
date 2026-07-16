package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// Typed wrappers over the raw JSON guest contract (setup/apply/view/legal).
// State is carried as opaque json.RawMessage — the host never needs to
// understand the engine's internal state shape, only route it back to the guest.

// ApplyResult mirrors the engine's ApplyResult<S>.
type ApplyResult struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error"`
	State  json.RawMessage `json:"state"`
	Events json.RawMessage `json:"events"`
}

func (g *GameRuntime) Setup(ctx context.Context, players []string, seed string, config json.RawMessage) (json.RawMessage, error) {
	cmd := map[string]any{"op": "setup", "players": players, "seed": seed}
	if len(config) > 0 {
		cmd["config"] = config
	}
	c, _ := json.Marshal(cmd)
	return g.Call(ctx, c)
}

func (g *GameRuntime) Apply(ctx context.Context, state, move json.RawMessage) (*ApplyResult, error) {
	cmd, _ := json.Marshal(map[string]any{"op": "apply", "state": state, "move": move})
	out, err := g.Call(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var res ApplyResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("decode apply result: %w (raw: %s)", err, truncate(out))
	}
	return &res, nil
}

func (g *GameRuntime) View(ctx context.Context, state json.RawMessage, playerID string) (json.RawMessage, error) {
	cmd, _ := json.Marshal(map[string]any{"op": "view", "state": state, "playerId": playerID})
	return g.Call(ctx, cmd)
}

// Legal enumerates moves. A non-empty playerID asks for THAT seat's moves
// (simultaneous stages, where several seats can act at once); empty means the
// current player. Older wasm ignores playerId and always answers for the current
// player, which is correct for single-seat games.
func (g *GameRuntime) Legal(ctx context.Context, state json.RawMessage, playerID string) (json.RawMessage, error) {
	cmd, _ := json.Marshal(map[string]any{"op": "legal", "state": state, "playerId": playerID})
	return g.Call(ctx, cmd)
}

// stateMeta is the minimal projection of the engine MatchState the host reads
// to drive the API (turn, end status, move count) without parsing game state.
type stateMeta struct {
	Flow struct {
		CurrentPlayer string   `json:"currentPlayer"`
		Active        []string `json:"active"` // simultaneous mode: seats allowed to act at once (nil/empty = ordinary turn)
		Turn          int      `json:"turn"`
		Phase         *string  `json:"phase"`
	} `json:"flow"`
	Ended  bool              `json:"ended"`
	Result json.RawMessage   `json:"result"`
	Log    []json.RawMessage `json:"log"`
}

func parseMeta(state json.RawMessage) (stateMeta, error) {
	var m stateMeta
	if err := json.Unmarshal(state, &m); err != nil {
		return m, fmt.Errorf("decode state meta: %w", err)
	}
	return m, nil
}

func (m stateMeta) moveCount() int { return len(m.Log) }

func truncate(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
