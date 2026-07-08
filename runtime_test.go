package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// wasmPath points at the artifact produced by:
//
//	docker compose -f infra/docker-compose.yml --profile wasm run --rm \
//	  wasm-build tools/wasm/build.sh games/hive dist/hive.wasm
const wasmPath = "../../dist/hive.wasm"

func loadRuntime(t *testing.T) *GameRuntime {
	t.Helper()
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Skipf("%s not built (%v); run the wasm build first", wasmPath, err)
	}
	rt, err := NewGameRuntime(context.Background(), wasm, 1024, 5*time.Second)
	if err != nil {
		t.Fatalf("NewGameRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return rt
}

func TestWasmSetupAndLegal(t *testing.T) {
	rt := loadRuntime(t)
	ctx := context.Background()

	// setup
	out, err := rt.Call(ctx, []byte(`{"op":"setup","players":["a","b"],"seed":"demo"}`))
	if err != nil {
		t.Fatalf("setup call: %v", err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(out, &state); err != nil {
		t.Fatalf("setup output not JSON: %v\n%s", err, out)
	}
	if _, ok := state["G"]; !ok {
		t.Fatalf("setup output missing G: %s", out)
	}

	// legal moves for the opening position: exactly 5 (one placement per kind at origin)
	legalCmd := append(append([]byte(`{"op":"legal","state":`), out...), '}')
	lout, err := rt.Call(ctx, legalCmd)
	if err != nil {
		t.Fatalf("legal call: %v", err)
	}
	var moves []map[string]json.RawMessage
	if err := json.Unmarshal(lout, &moves); err != nil {
		t.Fatalf("legal output not JSON array: %v\n%s", err, lout)
	}
	if len(moves) != 5 {
		t.Fatalf("expected 5 opening placements, got %d: %s", len(moves), lout)
	}
}

func TestWasmApplyIsAuthoritative(t *testing.T) {
	rt := loadRuntime(t)
	ctx := context.Background()

	setup, err := rt.Call(ctx, []byte(`{"op":"setup","players":["a","b"],"seed":"demo"}`))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Apply a legal opening placement (Ant at origin).
	applyCmd := append(append([]byte(`{"op":"apply","state":`), setup...),
		[]byte(`,"move":{"type":"place","playerId":"a","payload":{"kind":"A","q":0,"r":0}}}`)...)
	out, err := rt.Call(ctx, applyCmd)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var res struct {
		OK    bool `json:"ok"`
		State struct {
			G struct {
				Placed []int `json:"placed"`
			} `json:"G"`
		} `json:"state"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("apply output not JSON: %v\n%s", err, out)
	}
	if !res.OK {
		t.Fatalf("expected move accepted: %s", out)
	}
	if res.State.G.Placed[0] != 1 {
		t.Fatalf("expected player 0 to have placed 1 piece, got %d", res.State.G.Placed[0])
	}

	// The guest must reject an illegal move (wrong player's turn).
	badCmd := append(append([]byte(`{"op":"apply","state":`), setup...),
		[]byte(`,"move":{"type":"place","playerId":"b","payload":{"kind":"A","q":0,"r":0}}}`)...)
	bout, err := rt.Call(ctx, badCmd)
	if err != nil {
		t.Fatalf("apply(bad): %v", err)
	}
	var bres struct {
		OK bool `json:"ok"`
	}
	_ = json.Unmarshal(bout, &bres)
	if bres.OK {
		t.Fatalf("expected out-of-turn move to be rejected: %s", bout)
	}
}
