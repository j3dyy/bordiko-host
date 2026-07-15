package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LocalVersion marks a runtime loaded from GAMES_DIR rather than pulled from the
// marketplace registry. Local builds have no published version to pin to.
const LocalVersion = "local"

// latestTTL is how long we trust a cached "what is the latest version of X"
// answer. Short enough that a publish goes live within seconds, long enough that
// a busy lobby doesn't hammer the registry — and it is only consulted when a
// match is CREATED, never per move.
const latestTTL = 30 * time.Second

// GameRegistry holds compiled GameRuntimes keyed by gameID@version. Games are
// loaded from a local directory at startup and, on a cache miss, fetched and
// compiled on demand from the marketplace registry service.
//
// Runtimes are cached per VERSION, not per game. That matters for two reasons:
// a freshly published update must reach new matches without a redeploy (keying
// by game id alone made the first-fetched build permanent), and a match already
// in flight must keep running the exact build it started on — swapping the
// reducer under a live move log would break determinism and replay. So the match
// records its version at creation and every later call resolves that same key.
type GameRegistry struct {
	mu       sync.RWMutex
	games    map[string]*GameRuntime // "gameID@version" -> compiled runtime
	latest   map[string]latestEntry  // gameID -> most recent published version
	memPages uint32
	timeout  time.Duration

	registryURL string
	http        *http.Client
	now         func() time.Time
}

type latestEntry struct {
	version string
	at      time.Time
}

func NewGameRegistry(memPages uint32, timeout time.Duration, registryURL string) *GameRegistry {
	return &GameRegistry{
		games:       make(map[string]*GameRuntime),
		latest:      make(map[string]latestEntry),
		memPages:    memPages,
		timeout:     timeout,
		registryURL: registryURL,
		http:        &http.Client{Timeout: 15 * time.Second},
		now:         time.Now,
	}
}

func runtimeKey(gameID, version string) string { return gameID + "@" + version }

// LoadDir loads every *.wasm file in dir, using the base filename (without the
// extension) as the game id. Local builds are registered under LocalVersion and
// are used when the registry has nothing published for that id.
func (r *GameRegistry) LoadDir(ctx context.Context, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wasm") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".wasm")
		if err := r.LoadFile(ctx, id, filepath.Join(dir, e.Name())); err != nil {
			return n, fmt.Errorf("load %s: %w", e.Name(), err)
		}
		n++
	}
	return n, nil
}

func (r *GameRegistry) LoadFile(ctx context.Context, gameID, path string) error {
	wasm, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	rt, err := NewGameRuntime(ctx, wasm, r.memPages, r.timeout)
	if err != nil {
		return err
	}
	r.put(runtimeKey(gameID, LocalVersion), rt)
	return nil
}

func (r *GameRegistry) get(key string) (*GameRuntime, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.games[key]
	return rt, ok
}

func (r *GameRegistry) put(key string, rt *GameRuntime) {
	r.mu.Lock()
	r.games[key] = rt
	r.mu.Unlock()
}

// Get reports whether a local build of gameID is loaded (used by tests and the
// GAMES_DIR startup log).
func (r *GameRegistry) Get(gameID string) (*GameRuntime, bool) {
	return r.get(runtimeKey(gameID, LocalVersion))
}

// LatestVersion asks the registry which version of gameID is currently
// published, memoised for latestTTL. Empty when there is no registry configured
// or the game is not published there.
func (r *GameRegistry) LatestVersion(ctx context.Context, gameID string) (string, bool) {
	if r.registryURL == "" {
		return "", false
	}
	r.mu.RLock()
	ent, ok := r.latest[gameID]
	r.mu.RUnlock()
	if ok && r.now().Sub(ent.at) < latestTTL {
		return ent.version, true
	}

	version := ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.registryURL+"/games/"+gameID+"/version", nil)
	if err == nil {
		if resp, err := r.http.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var body struct {
					Version string `json:"version"`
				}
				if json.NewDecoder(resp.Body).Decode(&body) == nil {
					version = body.Version
				}
			}
		}
	}
	if version == "" {
		// Deliberately NOT cached. Remembering "not published" would make a game
		// unplayable for a whole TTL after it IS published — which is the exact
		// staleness this pinning exists to remove. An unpublished id just costs
		// one cheap 404 per match creation, and match creation is rare.
		return "", false
	}
	r.mu.Lock()
	r.latest[gameID] = latestEntry{version: version, at: r.now()}
	r.mu.Unlock()
	return version, true
}

// Resolve returns the runtime for an EXISTING match: the exact version it was
// created with. An empty version means the match predates version pinning, so
// fall back to the local build and then to whatever is published now.
func (r *GameRegistry) Resolve(ctx context.Context, gameID, version string) (*GameRuntime, bool) {
	if version == "" {
		if rt, ok := r.get(runtimeKey(gameID, LocalVersion)); ok {
			return rt, true
		}
		rt, _, ok := r.ResolveLatest(ctx, gameID)
		return rt, ok
	}
	if version == LocalVersion {
		return r.get(runtimeKey(gameID, LocalVersion))
	}
	if rt, ok := r.get(runtimeKey(gameID, version)); ok {
		return rt, true
	}
	return r.fetchVersion(ctx, gameID, version)
}

// ResolveLatest returns the runtime a NEW match should use, plus the version to
// pin onto that match. Prefers what the registry currently publishes; falls back
// to a local build when there's no registry (dev) or the game isn't published.
func (r *GameRegistry) ResolveLatest(ctx context.Context, gameID string) (*GameRuntime, string, bool) {
	if version, ok := r.LatestVersion(ctx, gameID); ok {
		if rt, ok := r.Resolve(ctx, gameID, version); ok {
			return rt, version, true
		}
	}
	// A registry that predates /games/{id}/version still stamps the version on
	// the unversioned wasm route, so pin from that instead. This keeps the two
	// services independently deployable in either order — without it, shipping
	// game-host first would 404 every match creation until the registry caught up.
	if rt, version, ok := r.fetchLatestWasm(ctx, gameID); ok {
		return rt, version, true
	}
	if rt, ok := r.get(runtimeKey(gameID, LocalVersion)); ok {
		return rt, LocalVersion, true
	}
	return nil, "", false
}

// fetchLatestWasm pulls whatever the registry currently publishes and reads the
// version back out of the response header.
func (r *GameRegistry) fetchLatestWasm(ctx context.Context, gameID string) (*GameRuntime, string, bool) {
	if r.registryURL == "" {
		return nil, "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.registryURL+"/games/"+gameID+"/wasm", nil)
	if err != nil {
		return nil, "", false
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", false
	}
	version := resp.Header.Get("X-Bordiko-Version")
	if version != "" {
		if rt, ok := r.get(runtimeKey(gameID, version)); ok {
			return rt, version, true // already compiled; don't do it twice
		}
	}
	wasm, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", false
	}
	rt, err := NewGameRuntime(ctx, wasm, r.memPages, r.timeout)
	if err != nil {
		return nil, "", false
	}
	if version == "" {
		// Unversioned: usable, but not pinnable. The match falls back to legacy
		// resolution, exactly as it behaved before pinning existed.
		return rt, "", true
	}
	r.put(runtimeKey(gameID, version), rt)
	return rt, version, true
}

// fetchVersion pulls one specific published build and compiles it.
func (r *GameRegistry) fetchVersion(ctx context.Context, gameID, version string) (*GameRuntime, bool) {
	if r.registryURL == "" {
		return nil, false
	}
	url := r.registryURL + "/games/" + gameID + "/versions/" + version + "/wasm"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	wasm, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	rt, err := NewGameRuntime(ctx, wasm, r.memPages, r.timeout)
	if err != nil {
		return nil, false
	}
	r.put(runtimeKey(gameID, version), rt)
	return rt, true
}

// IDs lists every game this host can serve — local builds plus anything it has
// fetched — deduped across versions.
func (r *GameRegistry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{}, len(r.games))
	ids := make([]string, 0, len(r.games))
	for key := range r.games {
		id := key
		if i := strings.LastIndex(key, "@"); i >= 0 {
			id = key[:i]
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
