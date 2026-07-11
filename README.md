# game-host

The authoritative brain. Loads published games as **sandboxed WASM** (wazero),
holds canonical match state, validates + applies every move by re-running the
game's deterministic guest, redacts per-player views, and event-sources the move
log to Postgres.

## How a move is authoritative

The server never trusts the client's state. For each request it takes the stored
canonical `MatchState`, hands it (plus the move) to the game's WASM guest, and
only commits if the guest accepts. The guest is the *same* deterministic engine
the SDK builds — it has no network, no filesystem, a memory cap, and a wall-clock
timeout. A move the guest rejects returns `422` and changes nothing.

## Config (env)

| Var | Default | Purpose |
| --- | --- | --- |
| `GAME_HOST_ADDR` | `:8081` | Listen address |
| `GAMES_DIR` | `dist` | Directory of built `*.wasm` games (id = filename) |
| `DATABASE_URL` | _(unset)_ | Postgres DSN; unset → in-memory store |
| `WASM_MEM_PAGES` | `1024` | Memory cap per instance (×64 KiB = 64 MiB) |
| `WASM_TIMEOUT_MS` | `5000` | Wall-clock cap per guest call |

## HTTP API

| Method & path | Body / query | Returns |
| --- | --- | --- |
| `GET /games` | — | `{games:[...]}` loaded game ids |
| `GET /stats` | — | `{counts:{<gameId>:n}}` — matches per game (the "plays" metric the marketplace catalog surfaces) |
| `POST /matches` | `{gameId, players[], seed?}` | match summary (`id`, `currentPlayer`, …) |
| `GET /matches/{id}` | — | match summary |
| `POST /matches/{id}/moves` | `{playerId, type, payload}` | `{ok, events, moveCount, currentPlayer, ended, result}` or `422 {ok:false,error}` |
| `GET /matches/{id}/view` | `?playerId=` | that player's **redacted** view |
| `GET /matches/{id}/legal` | — | `{currentPlayer, moves[]}` |
| `GET /leaderboard` | `?gameId=` | per-game ELO ladder `{entries:[{player,rating,wins,losses,draws,games}]}` |

When a match ends, the game-host records the result and updates a per-game
`ratings` ladder (head-to-head ELO, K=32, base 1200; win/loss counts for >2
players). `player` is the user id passed at match creation, so the gateway
resolves display names for the public leaderboard. Ratings persist in Postgres
when `DATABASE_URL` is set, else in memory.

## Run locally

```bash
# 1. Build a game to WASM (writes ./dist/hive.wasm):
docker compose -f infra/docker-compose.yml --profile wasm run --rm \
  wasm-build tools/wasm/build.sh games/hive dist/hive.wasm

# 2a. In-memory:
GAMES_DIR=dist go run github.com/bordiko/gamehost

# 2b. With Postgres persistence:
docker compose -f infra/docker-compose.yml --profile infra up -d postgres
DATABASE_URL='postgres://bordiko:bordiko@localhost:5432/bordiko?sslmode=disable' \
  GAMES_DIR=dist go run github.com/bordiko/gamehost

# 3. Play a move:
curl -s -X POST localhost:8081/matches \
  -d '{"gameId":"hive","players":["white","black"]}'
```

## Tests

`go test github.com/bordiko/gamehost` — exercises the wazero runner against
`dist/hive.wasm` (setup / legal / apply / out-of-turn rejection). Tests skip if
the wasm hasn't been built yet.

## Notes / next

- Sandbox limits today are **memory cap + wall-clock timeout**. Deterministic
  *fuel* metering (a hard instruction budget) is not built into wazero; it would
  require a gas-metering pass or a switch to wasmtime. Tracked for hardening.
- Phase 4 puts the gateway (WebSocket) in front of this API for real-time play.
