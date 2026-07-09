// Command game-host is the authoritative brain of Bordiko.
//
// It loads published games as sandboxed WASM (wazero), holds canonical match
// state, validates + applies every move by re-running the game's deterministic
// guest, redacts per-player views, and event-sources the move log. Randomness
// and time are the guest's only nondeterminism and are host-controlled; the
// guest has no network or filesystem access.
//
// Config (env):
//
//	GAME_HOST_ADDR   listen address              (default ":8081")
//	GAMES_DIR        directory of *.wasm games   (default "dist")
//	DATABASE_URL     Postgres DSN; if unset, an in-memory store is used
//	WASM_MEM_PAGES   memory cap per instance     (default 1024 = 64 MiB)
//	WASM_TIMEOUT_MS  wall-clock cap per call     (default 5000)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	ctx := context.Background()

	registryURL := os.Getenv("REGISTRY_URL")
	games := NewGameRegistry(uint32(envInt("WASM_MEM_PAGES", 1024)),
		time.Duration(envInt("WASM_TIMEOUT_MS", 5000))*time.Millisecond, registryURL)
	gamesDir := env("GAMES_DIR", "dist")
	n, err := games.LoadDir(ctx, gamesDir)
	if err != nil {
		log.Printf("note: no local games from %q: %v", gamesDir, err)
	}
	log.Printf("loaded %d local game(s) from %q: %v", n, gamesDir, games.IDs())
	if registryURL != "" {
		log.Printf("registry fallback enabled: %s (unknown games fetched on demand)", registryURL)
	}

	store, err := openStore(ctx)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	ratings, err := openRatings(ctx, store)
	if err != nil {
		log.Fatalf("ratings: %v", err)
	}
	defer ratings.Close()

	srv := NewServer(games, store, ratings)
	addr := env("GAME_HOST_ADDR", ":8081")
	log.Printf("bordiko game-host listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.Routes()); err != nil {
		log.Fatalf("game-host failed: %v", err)
	}
}

func openStore(ctx context.Context) (Store, error) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		log.Printf("using Postgres store")
		return NewPostgresStore(ctx, url)
	}
	log.Printf("using in-memory store (set DATABASE_URL for persistence)")
	return NewMemoryStore(), nil
}

// openRatings shares the match store's Postgres pool when durable, otherwise
// keeps ratings in memory.
func openRatings(ctx context.Context, store Store) (RatingsStore, error) {
	if ps, ok := store.(*PostgresStore); ok {
		log.Printf("using Postgres ratings")
		return NewPostgresRatings(ctx, ps.pool)
	}
	log.Printf("using in-memory ratings")
	return NewMemoryRatings(), nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
