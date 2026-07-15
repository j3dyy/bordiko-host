package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeRegistry serves just the version endpoint, counting how often it is asked.
func fakeRegistry(version *string, hits *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/games/avalon/version" {
			http.NotFound(w, r)
			return
		}
		*hits++
		if *version == "" {
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gameId":"avalon","version":"` + *version + `","sha":"deadbeef"}`))
	}))
}

// A published version is memoised for latestTTL, then re-checked — so an update
// lands on new matches without a redeploy, but a busy lobby doesn't hammer the
// registry.
func TestLatestVersionMemoisesThenRefreshes(t *testing.T) {
	version := "0.1.0"
	hits := 0
	srv := fakeRegistry(&version, &hits)
	defer srv.Close()

	now := time.Now()
	r := NewGameRegistry(64, time.Second, srv.URL)
	r.now = func() time.Time { return now }

	if got, ok := r.LatestVersion(context.Background(), "avalon"); !ok || got != "0.1.0" {
		t.Fatalf("first lookup: got %q/%v, want 0.1.0/true", got, ok)
	}
	// Repeat lookups inside the TTL are served from the memo.
	for i := 0; i < 3; i++ {
		if got, _ := r.LatestVersion(context.Background(), "avalon"); got != "0.1.0" {
			t.Fatalf("memoised lookup: got %q, want 0.1.0", got)
		}
	}
	if hits != 1 {
		t.Fatalf("registry asked %d times inside the TTL, want 1", hits)
	}

	// A new build is published; the memo must not outlive its TTL.
	version = "0.2.0"
	if got, _ := r.LatestVersion(context.Background(), "avalon"); got != "0.1.0" {
		t.Fatalf("inside TTL the old version should still be served, got %q", got)
	}
	now = now.Add(latestTTL + time.Second)
	if got, ok := r.LatestVersion(context.Background(), "avalon"); !ok || got != "0.2.0" {
		t.Fatalf("after the TTL: got %q/%v, want 0.2.0/true — a publish must reach new matches", got, ok)
	}
}

// A game that ISN'T published yet must not be remembered as unpublished:
// negative caching would keep it unplayable for a whole TTL after it goes live,
// which is exactly the staleness this cache exists to avoid.
func TestLatestVersionDoesNotCacheMisses(t *testing.T) {
	version := "" // nothing published
	hits := 0
	srv := fakeRegistry(&version, &hits)
	defer srv.Close()

	now := time.Now()
	r := NewGameRegistry(64, time.Second, srv.URL)
	r.now = func() time.Time { return now }

	if _, ok := r.LatestVersion(context.Background(), "avalon"); ok {
		t.Fatal("unpublished game reported as published")
	}
	// Publish it — with the clock UNMOVED, so only a non-cached miss can see it.
	version = "0.1.0"
	if got, ok := r.LatestVersion(context.Background(), "avalon"); !ok || got != "0.1.0" {
		t.Fatalf("a freshly published game must be visible at once: got %q/%v", got, ok)
	}
}

// With no registry configured (pure local dev), the host must not pretend a
// version exists — it falls back to the GAMES_DIR build.
func TestLatestVersionWithoutRegistry(t *testing.T) {
	r := NewGameRegistry(64, time.Second, "")
	if v, ok := r.LatestVersion(context.Background(), "avalon"); ok || v != "" {
		t.Fatalf("no registry: got %q/%v, want \"\"/false", v, ok)
	}
}

// Runtimes are keyed by gameID@version, so two versions of one game coexist —
// that is what lets an in-flight match keep its build while new matches take the
// update. IDs() still reports one entry per game.
func TestRuntimeKeyingIsPerVersionAndIDsDedupe(t *testing.T) {
	r := NewGameRegistry(64, time.Second, "")
	rt1 := &GameRuntime{}
	rt2 := &GameRuntime{}
	r.put(runtimeKey("avalon", "0.1.0"), rt1)
	r.put(runtimeKey("avalon", "0.2.0"), rt2)

	if got, _ := r.get(runtimeKey("avalon", "0.1.0")); got != rt1 {
		t.Fatal("0.1.0 did not resolve to its own runtime")
	}
	if got, _ := r.get(runtimeKey("avalon", "0.2.0")); got != rt2 {
		t.Fatal("0.2.0 did not resolve to its own runtime")
	}
	if ids := r.IDs(); len(ids) != 1 || ids[0] != "avalon" {
		t.Fatalf("IDs() = %v, want exactly [avalon]", ids)
	}
}
