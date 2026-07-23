package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
)

// hasBot reports whether any of the match's players is a bot (id prefixed
// "bot:"). Bot-containing matches are excluded from ratings/leaderboards.
func hasBot(players []string) bool {
	for _, p := range players {
		if strings.HasPrefix(p, "bot:") {
			return true
		}
	}
	return false
}

// Server is the authoritative game-host HTTP API. It owns the game registry (the
// sandboxed WASM runtimes) and the match Store, and it is the only component
// allowed to advance a match — every move is validated by re-running the game's
// deterministic guest.
type Server struct {
	games   *GameRegistry
	store   Store
	ratings RatingsStore
	// Per-match write lock. A real-time match takes many concurrent writes at once
	// (the 20 Hz tick clock + every player's input moves), and each write is a
	// load→apply→append read-modify-write on the move log. Without serialising per
	// match, two concurrent writes read the same move count and insert the same
	// primary key → "duplicate key ... moves_pkey". This makes each match's writes
	// atomic; different matches never contend.
	locks sync.Map // matchID → *sync.Mutex
}

func NewServer(games *GameRegistry, store Store, ratings RatingsStore) *Server {
	return &Server{games: games, store: store, ratings: ratings}
}

// lockMatch returns (already locked) the mutex guarding one match's writes.
// The caller must Unlock it (defer).
func (s *Server) lockMatch(id string) *sync.Mutex {
	mu, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /readyz", s.health)
	mux.HandleFunc("GET /games", s.listGames)
	mux.HandleFunc("GET /stats", s.getStats)
	mux.HandleFunc("GET /players/{id}/active", s.getActiveMatch)
	mux.HandleFunc("GET /leaderboard", s.getLeaderboard)
	mux.HandleFunc("POST /matches", s.createMatch)
	mux.HandleFunc("GET /matches/{id}", s.getMatch)
	mux.HandleFunc("POST /matches/{id}/moves", s.applyMove)
	mux.HandleFunc("POST /matches/{id}/tick", s.tickMatch)
	mux.HandleFunc("POST /matches/{id}/end", s.endMatch)
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

// getStats returns per-game match counts — the "plays" metric surfaced by the
// marketplace catalog (via the gateway).
// buildVersion is bumped on notable game-host changes so a deploy can be
// verified live (GET /api/stats → "version"), removing the "did it actually
// rebuild?" ambiguity. Also forces a real source change that busts Docker's
// build cache.
const buildVersion = "2026-07-15-version-pinning"

func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.CountsByGame(r.Context())
	if err != nil {
		counts = map[string]int{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": counts, "version": buildVersion})
}

// getActiveMatch reports whether a player is currently in an unfinished match
// (so the gateway can enforce one game at a time and offer "resume").
func (s *Server) getActiveMatch(w http.ResponseWriter, r *http.Request) {
	id, gameID, ok, err := s.store.ActiveMatchForPlayer(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": ok, "matchId": id, "gameId": gameID})
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
	GameID  string          `json:"gameId"`
	Players []string        `json:"players"`
	Seed    string          `json:"seed"`
	Config  json.RawMessage `json:"config"`
}

func (s *Server) createMatch(w http.ResponseWriter, r *http.Request) {
	var req createMatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	// A new match takes whatever is published NOW, and pins it — later moves
	// resolve this exact version, so a publish mid-match can't swap the reducer.
	rt, version, ok := s.games.ResolveLatest(r.Context(), req.GameID)
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

	state, err := rt.Setup(r.Context(), req.Players, seed, req.Config)
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
		ID:          newID(),
		GameID:      req.GameID,
		GameVersion: version,
		Seed:        seed,
		Players:     req.Players,
		State:       state,
		MoveCount:   meta.moveCount(),
		Ended:       meta.Ended,
		Result:      meta.Result,
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
	defer s.lockMatch(r.PathValue("id")).Unlock()
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
	rt, ok := s.games.Resolve(r.Context(), m.GameID, m.GameVersion)
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
	// the single point at which we record the result on the leaderboard. Matches
	// that include a bot don't count toward ratings (they'd pollute the boards).
	if meta.Ended && !hasBot(m.Players) {
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

type tickRequest struct {
	DtMs int `json:"dt"` // fixed timestep in ms (host clock = 1000/tickRate)
}

// tickMatch advances a real-time match's world by one fixed timestep. It is the
// system-driven twin of applyMove: no player, no legality check — the gateway
// clock calls it at the game's tickRate. Only games whose reducer has a `tick`
// handler accept it (others return 422 "not real-time"), so a stray tick against
// a turn-based match is a harmless no-op error, never a mutation.
func (s *Server) tickMatch(w http.ResponseWriter, r *http.Request) {
	defer s.lockMatch(r.PathValue("id")).Unlock()
	m, _, ok := s.loadMatch(w, r)
	if !ok {
		return
	}
	if m.Ended {
		writeError(w, http.StatusConflict, "match_ended", "the match has already ended")
		return
	}
	var req tickRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.DtMs <= 0 {
		writeError(w, http.StatusBadRequest, "bad_dt", "dt must be a positive number of milliseconds")
		return
	}
	rt, ok := s.games.Resolve(r.Context(), m.GameID, m.GameVersion)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unknown_game", m.GameID)
		return
	}

	res, err := rt.Tick(r.Context(), m.State, req.DtMs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "guest_error", err.Error())
		return
	}
	if !res.OK {
		// e.g. a turn-based game with no tick handler — client/config error, not a fault.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": res.Error})
		return
	}

	meta, err := parseMeta(res.State)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bad_state", err.Error())
		return
	}
	tick, _ := json.Marshal(map[string]any{"type": "__tick", "dt": req.DtMs})
	if err := s.store.AppendMove(r.Context(), m.ID, tick, res.State, meta.Result, meta.moveCount(), meta.Ended); err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}

	// A tick can end the match (e.g. someone was pushed out). Record the result on
	// the leaderboard exactly once, same as applyMove.
	if meta.Ended && !hasBot(m.Players) {
		if result, ok := parseResult(meta.Result); ok {
			if err := s.ratings.RecordResult(r.Context(), m.GameID, m.Players, result); err != nil {
				log.Printf("record tick result for match %s: %v", m.ID, err)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"events":    res.Events,
		"moveCount": meta.moveCount(),
		"ended":     meta.Ended,
		"result":    meta.Result,
	})
}

type endMatchRequest struct {
	Result json.RawMessage `json:"result"`
	By     string          `json:"by"` // the player who forfeited / left (for the audit move)
}

// endMatch force-ends a match with a caller-supplied result — used by the
// gateway when a player leaves (their team forfeits) so the others aren't stuck.
// It patches `ended`+`result` INTO the state JSON (matchSummary/view read the
// state, not the stored flag), records the audit move, and updates ratings. It
// is idempotent: ending an already-ended match just returns its summary.
func (s *Server) endMatch(w http.ResponseWriter, r *http.Request) {
	defer s.lockMatch(r.PathValue("id")).Unlock()
	m, meta, ok := s.loadMatch(w, r)
	if !ok {
		return
	}
	if meta.Ended {
		writeJSON(w, http.StatusOK, matchSummary(m, meta))
		return
	}
	var req endMatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Result) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "result required")
		return
	}

	// Mark the canonical state ended with the given result so view/summary report
	// it consistently everywhere.
	var state map[string]json.RawMessage
	if err := json.Unmarshal(m.State, &state); err != nil {
		writeError(w, http.StatusInternalServerError, "bad_state", err.Error())
		return
	}
	state["ended"] = json.RawMessage("true")
	state["result"] = req.Result
	newState, err := json.Marshal(state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bad_state", err.Error())
		return
	}

	forfeit, _ := json.Marshal(map[string]any{"type": "__end", "playerId": req.By})
	if err := s.store.AppendMove(r.Context(), m.ID, forfeit, newState, req.Result, m.MoveCount+1, true); err != nil {
		writeError(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	if result, ok := parseResult(req.Result); ok && !hasBot(m.Players) {
		if err := s.ratings.RecordResult(r.Context(), m.GameID, m.Players, result); err != nil {
			log.Printf("record forfeit result for match %s: %v", m.ID, err)
		}
	}

	newMeta, _ := parseMeta(newState)
	m.State = newState
	writeJSON(w, http.StatusOK, matchSummary(m, newMeta))
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
	rt, ok := s.games.Resolve(r.Context(), m.GameID, m.GameVersion)
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
	rt, ok := s.games.Resolve(r.Context(), m.GameID, m.GameVersion)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unknown_game", m.GameID)
		return
	}
	// Optional ?playerId= asks for that seat's moves (simultaneous stages);
	// otherwise the current player's.
	playerID := r.URL.Query().Get("playerId")
	moves, err := rt.Legal(r.Context(), m.State, playerID)
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
		"gameVersion":   m.GameVersion,
		"players":       m.Players,
		"currentPlayer": meta.Flow.CurrentPlayer,
		"active":        meta.Flow.Active, // simultaneous mode: seats allowed to act at once
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
