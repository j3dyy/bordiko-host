package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// GameRuntime runs one game's sandboxed WASM guest.
//
// The guest is a self-contained WASI module (QuickJS + engine + game, built by
// tools/wasm). It follows a one-shot contract: read a single JSON command from
// stdin, write a single JSON result to stdout, exit. We compile the module once
// and instantiate a fresh, isolated instance per call — so no state leaks
// between moves and a misbehaving move cannot corrupt another.
//
// Sandbox enforcement:
//   - memory is capped (WithMemoryLimitPages),
//   - each call is bounded by a wall-clock timeout (context + CloseOnContextDone),
//   - the guest has NO imports beyond WASI stdio: no network, no filesystem,
//     no clock beyond what the deterministic engine already forbids using.
type GameRuntime struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	timeout  time.Duration
}

// NewGameRuntime compiles a game.wasm and prepares it for repeated calls.
func NewGameRuntime(ctx context.Context, wasm []byte, memPages uint32, timeout time.Duration) (*GameRuntime, error) {
	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(memPages)
	r := wazero.NewRuntimeWithConfig(ctx, cfg)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	compiled, err := r.CompileModule(ctx, wasm)
	if err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("compile wasm: %w", err)
	}
	return &GameRuntime{runtime: r, compiled: compiled, timeout: timeout}, nil
}

// Close releases the runtime and all compiled modules.
func (g *GameRuntime) Close(ctx context.Context) error {
	return g.runtime.Close(ctx)
}

// Call executes one guest command and returns its JSON output.
func (g *GameRuntime) Call(ctx context.Context, command []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous → allows many concurrent instances
		WithStdin(bytes.NewReader(command)).
		WithStdout(&stdout).
		WithStderr(&stderr).
		WithStartFunctions() // instantiate without running; we call _start below

	mod, err := g.runtime.InstantiateModule(ctx, g.compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	defer mod.Close(ctx)

	start := mod.ExportedFunction("_start")
	if start == nil {
		return nil, errors.New("wasm guest has no _start export")
	}

	if _, err := start.Call(ctx); err != nil {
		var exitErr *sys.ExitError
		if errors.As(err, &exitErr) {
			if code := exitErr.ExitCode(); code != 0 {
				return nil, fmt.Errorf("guest exited with code %d: %s", code, stderr.String())
			}
			// exit code 0 is normal termination for a WASI command.
		} else {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("guest exceeded time/resource limit: %w", ctx.Err())
			}
			return nil, fmt.Errorf("guest trap: %w (stderr: %s)", err, stderr.String())
		}
	}
	return stdout.Bytes(), nil
}
