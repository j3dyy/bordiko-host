package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// An OLD registry: no /games/{id}/version route, but — like every registry we
// have ever deployed — it stamps X-Bordiko-Version on the wasm it serves.
func oldRegistry(t *testing.T, wasm []byte, version string, versionHits, wasmHits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/games/tic-tac-toe/version":
			*versionHits++
			http.NotFound(w, r) // the route does not exist on this build
		case "/games/tic-tac-toe/wasm":
			*wasmHits++
			w.Header().Set("Content-Type", "application/wasm")
			w.Header().Set("X-Bordiko-Version", version)
			_, _ = w.Write(wasm)
		default:
			http.NotFound(w, r)
		}
	}))
}

// game-host must stay deployable BEFORE the registry: when /version 404s it
// falls back to the unversioned wasm route and pins from the header, rather than
// failing every match creation.
func TestResolveLatestFallsBackToOldRegistry(t *testing.T) {
	// Reuse the built hive artifact — this test cares about the fetch/pin path,
	// not which game it is.
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Skipf("%s not built (%v); run the wasm build first", wasmPath, err)
	}

	versionHits, wasmHits := 0, 0
	srv := oldRegistry(t, wasm, "0.9.9", &versionHits, &wasmHits)
	defer srv.Close()

	r := NewGameRegistry(1024, 5*time.Second, srv.URL)
	rt, version, ok := r.ResolveLatest(context.Background(), "tic-tac-toe")
	if !ok || rt == nil {
		t.Fatal("ResolveLatest failed against a registry with no /version route")
	}
	if version != "0.9.9" {
		t.Fatalf("pinned version = %q, want 0.9.9 (read from X-Bordiko-Version)", version)
	}
	if versionHits == 0 {
		t.Fatal("expected the cheap /version route to be tried first")
	}

	// Having learned the version, the runtime is cached under it — so resolving
	// that same match again is a cache hit, not another megabyte download.
	before := wasmHits
	rt2, ok := r.Resolve(context.Background(), "tic-tac-toe", "0.9.9")
	if !ok || rt2 != rt {
		t.Fatal("pinned version did not resolve to the already-compiled runtime")
	}
	if wasmHits != before {
		t.Fatalf("re-resolving a pinned version re-downloaded the wasm (%d -> %d)", before, wasmHits)
	}
}
