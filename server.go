package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// Server is the authoritative game-host HTTP API. It owns the game registry (the
// sandboxed WASM runtimes) and the match Store, and it is the only component
// allowed to advance a match — every move is validated by re-running the game's
// deterministic guest.
type Server struct {
	games   *GameRegistry
	store   Store
	ratings RatingsStore
}

func NewServer(games *GameRegistry, store Store, ratings RatingsStore) *Server {
	return &Server{games: games, store: store, ratings: ratings}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /readyz", s.health)
	mux.HandleFunc("GET /games", s.listGames)
	mux.HandleFunc("GET /leaderboard", s.getLeaderboard)
	mux.HandleFunc("POST /matches", s.createMatch)
	mux.HandleFunc("GET /matches/{id}", s.getMatch)
	mux.HandleFunc("POST /matches/{id}/moves", s.applyMove)
	mux.HandleFunc("GET /matches/{id}/view", s.getView)
	mux.HandleFunc("GET /matches/{id}/legal", s.getLegal)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"service": "game-host", "status": "ok"})
}

func (s *Server) listGames(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"games": s.games.IDs()})
}

// getLeaderboard returns the per-game rating ladder (player = user id). The
// gateway enriches these rows with display names.
func (s *Server) getLeaderboard(w http.ResponseWriter, r *http.Request) {
	gameID := r.URL.Query().Get("gameId")
	entries, err := s.ratings.Leaderboard(r.Context(), gameID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ratings_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gameId": gameID, "entries": entries})
}

type createMatchRequest struct {
	GameID  string   `json:"gameId"`
	Players []string `json:"players"`
	Seed    string   `json:"seed"`
}

func (s *Server) createMatch(w http.ResponseWriter, r *http.Request) {
	var req createMatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	rt, ok := s.games.GetOrFetch(r.Context(), req.GameID)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_game", "no such game: "+req.GameID)
		return
	}
	if len(req.Players) < 2 {
		writeError(w, http.StatusBadRequest, "bad_players", "at least two players required")
		return
	}
	seed := req.Seed
	if seed == "" {
		seed = randomSeed()
	}

	state, err := rt.Setup(r.Context(), req.Players, seed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "setup_failed", err.Error())
		return
	}
	meta, err := parseMeta(state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bad_state", err.Error())
		return
	}

	m := &Match{
		ID:        newID(),
		GameID:    req.GameID,
		Seed:      seed,
		Players:   req.Players,
		State:     state,
		MoveCount: meta.moveCount(),
		Ended:     meta.Ended,
		Result:    meta.Result,
	}
	if err := s.store.CreateMatch(r.Context(), m); err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, matchSummary(m, meta))
}

func (s *Server) getMatch(w http.ResponseWriter, r *http.Request) {
	m, meta, ok := s.loadMatch(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, matchSummary(m, meta))
}

type applyMoveRequest struct {
	PlayerID string          `json:"playerId"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
}

func (s *Server) applyMove(w http.ResponseWriter, r *http.Request) {
	m, _, ok := s.loadMatch(w, r)
	if !ok {
		return
	}
	if m.Ended {
		writeError(w, http.StatusConflict, "match_ended", "the match has already ended")
		return
	}
	var req applyMoveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	rt, ok := s.games.GetOrFetch(r.Context(), m.GameID)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unknown_game", m.GameID)
		return
	}

	move, _ := json.Marshal(map[string]any{
		"type":     req.Type,
		"playerId": req.PlayerID,
		"payload":  req.Payload,
	})
	res, err := rt.Apply(r.Context(), m.State, move)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "guest_error", err.Error())
		return
	}
	if !res.OK {
		// A rejected move is a normal client error, not a server fault.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": res.Error})
		return
	}

	meta, err := parseMeta(res.State)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bad_state", err.Error())
		return
	}
	if err := s.store.AppendMove(r.Context(), m.ID, move, res.State, meta.Result, meta.moveCount(), meta.Ended); err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}

	// A match ends exactly once (further moves are rejected above), so this is
	// the single point at which we record the result on the leaderboard.
	if meta.Ended {
		if result, ok := parseResult(meta.Result); ok {
			if err := s.ratings.RecordResult(r.Context(), m.GameID, m.Players, result); err != nil {
				log.Printf("record result for match %s: %v", m.ID, err)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"events":        res.Events,
		"moveCount":     meta.moveCount(),
		"currentPlayer": meta.Flow.CurrentPlayer,
		"ended":         meta.Ended,
		"result":        meta.Result,
	})
}

func (s *Server) getView(w http.ResponseWriter, r *http.Request) {
	m, _, ok := s.loadMatch(w, r)
	if !ok {
		return
	}
	playerID := r.URL.Query().Get("playerId")
	if playerID == "" {
		writeError(w, http.StatusBadRequest, "missing_player", "playerId query param required")
		return
	}
	rt, ok := s.games.GetOrFetch(r.Context(), m.GameID)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unknown_game", m.GameID)
		return
	}
	view, err := rt.View(r.Context(), m.State, playerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "guest_error", err.Error())
		return
	}
	writeRaw(w, http.StatusOK, view)
}

func (s *Server) getLegal(w http.ResponseWriter, r *http.Request) {
	m, meta, ok := s.loadMatch(w, r)
	if !ok {
		return
	}
	rt, ok := s.games.GetOrFetch(r.Context(), m.GameID)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unknown_game", m.GameID)
		return
	}
	moves, err := rt.Legal(r.Context(), m.State)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "guest_error", err.Error())
		return
	}
	writeRaw(w, http.StatusOK, mustJSON(map[string]any{
		"currentPlayer": meta.Flow.CurrentPlayer,
		"moves":         moves,
	}))
}

// loadMatch fetches the match named in the path and its parsed meta, writing the
// appropriate error response and returning ok=false on failure.
func (s *Server) loadMatch(w http.ResponseWriter, r *http.Request) (*Match, stateMeta, bool) {
	id := r.PathValue("id")
	m, err := s.store.GetMatch(r.Context(), id)
	if errors.Is(err, ErrMatchNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such match")
		return nil, stateMeta{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return nil, stateMeta{}, false
	}
	meta, err := parseMeta(m.State)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bad_state", err.Error())
		return nil, stateMeta{}, false
	}
	return m, meta, true
}

func matchSummary(m *Match, meta stateMeta) map[string]any {
	return map[string]any{
		"id":            m.ID,
		"gameId":        m.GameID,
		"players":       m.Players,
		"currentPlayer": meta.Flow.CurrentPlayer,
		"turn":          meta.Flow.Turn,
		"moveCount":     meta.moveCount(),
		"ended":         meta.Ended,
		"result":        meta.Result,
	}
}

/* ------------------------------- utilities -------------------------------- */

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeRaw(w, status, mustJSON(v))
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "message": message})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"error":"marshal_failed"}`)
	}
	return b
}

func newID() string      { return randHex(12) }
func randomSeed() string { return randHex(8) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand.Read essentially never fails; fall back to a fixed value.
		return "seed"
	}
	return hex.EncodeToString(b)
}
