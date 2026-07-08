package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GameRegistry holds one compiled GameRuntime per published game id. Games are
// loaded from a local directory at startup and, on a cache miss, fetched and
// compiled on demand from the marketplace registry service.
type GameRegistry struct {
	mu          sync.RWMutex
	games       map[string]*GameRuntime
	memPages    uint32
	timeout     time.Duration
	registryURL string
	http        *http.Client
}

func NewGameRegistry(memPages uint32, timeout time.Duration, registryURL string) *GameRegistry {
	return &GameRegistry{
		games:       make(map[string]*GameRuntime),
		memPages:    memPages,
		timeout:     timeout,
		registryURL: registryURL,
		http:        &http.Client{Timeout: 15 * time.Second},
	}
}

// LoadDir loads every *.wasm file in dir, using the base filename (without the
// extension) as the game id. In Phase 6 this is superseded by pulling versioned
// packages from the registry service; for now it's a directory of built games.
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
	r.mu.Lock()
	r.games[gameID] = rt
	r.mu.Unlock()
	return nil
}

func (r *GameRegistry) Get(gameID string) (*GameRuntime, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.games[gameID]
	return rt, ok
}

// GetOrFetch returns the runtime for gameID, fetching and compiling it from the
// marketplace registry on a cache miss. This is what makes a freshly-published
// game playable without redeploying the game-host.
func (r *GameRegistry) GetOrFetch(ctx context.Context, gameID string) (*GameRuntime, bool) {
	if rt, ok := r.Get(gameID); ok {
		return rt, true
	}
	if r.registryURL == "" {
		return nil, false
	}
	url := r.registryURL + "/games/" + gameID + "/wasm"
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
	r.mu.Lock()
	r.games[gameID] = rt
	r.mu.Unlock()
	return rt, true
}

func (r *GameRegistry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.games))
	for id := range r.games {
		ids = append(ids, id)
	}
	return ids
}
