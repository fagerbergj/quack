# sleeper

Sleeper fantasy-football extension for quack. The client slice (issue #93) shipped the OpenAPI
spec, generated client, recorded fixtures, and a cached wrapper. This slice adds the extension
UI (see `~/workspace/wt/sleeper-requirements.md` for the full extension design) - tools/skills
land separately.

## UI

`sleeper/ui.go` mounts a vanilla HTML/CSS/JS page (embedded from `sleeper/ui/static/`, no build
step) on the authed router at `GET /sleeper/`, plus four JSON routes:

- `GET /sleeper/api/seasons?league_id=` - the season chain (walks `previous_league_id`,
  tolerating a gap deeper in history instead of erroring the whole list), each with this
  league's record for the configured `default_user`.
- `GET /sleeper/api/season?league_id=` - one season's live league facts, "me"/opponent (from
  rosters + the current week's matchups), standings, recent moves, and reserve-slot count -
  built from the cached Sleeper client, never from job artifacts.
- `GET /sleeper/api/artifacts?league_id=&stop=` - every job artifact for that stop
  (`draft`, a week number, or `review`), season notes, and per-partner trade talks, each read
  via `Host.ReadArtifact` by its `ext:sleeper:<league>:<stop>:<job>` chat id. Until a job has
  run, its card is `found: false`.
- `POST /sleeper/api/jobs` `{league_id, stop, job, args}` - calls `Host.Dispatch` with that same
  chat id (a repeat call appends a turn) and returns the chat link.

Artifact JSON schemas live in `sleeper/ui/schemas/*.json` (one per job: lineup, waivers, trade,
digest, trends, retro, draft, history, season-notes) - the renderer in `sleeper/ui/static/
render.js` reads exactly those shapes. `sleeper/ui/fixtures/*.json` are schema-conformant
examples derived from the approved prototype's data (`~/workspace/wt/sleeper-ui-ref/`).

### Storybook

`sleeper/ui/storybook/` is a standalone `@storybook/html-vite` project (pinned exact versions;
`npm install` then `npm run storybook` for `-p 6011`, `npm run build-storybook`). Stories live in
`stories/*.stories.js`, one per card plus `FullPage`, each with `Default`/`Dark`/
`MobileViewport390` variants, fed from `sleeper/ui/fixtures/`. `npm run render-check` builds
Storybook and screenshots every story at 390/1280 width, light/dark, with Playwright, failing on
a console error or horizontal overflow; CI runs it as the `sleeper-storybook` job.

## Plugin

`plugin/` is the Sleeper plugin quack loads through its plugin registry: `plugin.json`, the seven
job agents under `agents/` (card, prompt, rubric, and `agent.yaml` with tools, skills, and model
role), the skills under `skills/`, and the ten workflow shapes under `workflows/`. quack seeds it as
`github:fagerbergj/quack-extensions@sleeper/vX.Y.Z#sleeper/plugin`, a registry row named `sleeper`,
and seeds its agents and workflows only while `extensions.sleeper` is configured.

`plugin_test.go` checks that every tool an `agent.yaml` names is one of this module's tools or a
quack builtin from its allowlist, that each agent ships a prompt, a rubric and a `sleeper:` skill,
that each card's artifact kind has a schema in `ArtifactSchemas`, and that each skill's
`references/*schema*.json` is byte-identical to the `ui/schemas/<kind>.json` quack validates the
agent's write against. Edit the ui schema and copy it over the reference. `tools/quack-compat.sh`
seeds `plugin/` into quack, fails unless every agent and workflow in `plugin.json` seeds, and fails
when a non-`sleeper_` tool is missing from quack's builtin tool map.

One `sleeper/vX.Y.Z` tag releases both halves. quack pins the Go tools in `go.mod` and the plugin in
its `plugins.seed` entry, and both pins name the same tag. `plugin.json`'s `version` is that tag's
version; bump it in the PR that precedes the tag.

1. Merge the change and tag `sleeper/vX.Y.Z`.
2. In quack, bump `go.mod` and the `plugins.seed` ref to that tag in one PR. A deployment with its
   own `plugins.seed` bumps its entry to the same tag along with the image.
3. Restart. quack marks the rows `plugins.seed` creates as seeded, and at boot a seeded row whose
   seed entry changed moves to the new entry and is fetched at the new tag. A row last set over REST
   (`POST /api/v1/plugins`) belongs to the operator and does not follow the seed.

The first cutover needs no manual step either. Before the move, deployments seeded
`.agents/plugins/sleeper` as a local row. Upgrade quack to the release that fetches this plugin and
change the deployment's seed entry to the `github:` form: boot replaces the local row, since config
owns local rows, and fetches the plugin. Do not delete the row by hand.

A plugin-only change (prompt, skill, rubric, workflow) can go live before the next image: bump the
deployment's seed ref and restart. When the change touches Go tools, the image carrying the new
`go.mod` must ship with it, since a plugin agent naming a tool the binary lacks is dropped.

## Decision points

`decisions.go` declares four observe-only points (`sdk.DecisionPoints`) for a paired shadow A/B of
the weekly calls, scored later against real points. They are asked from `RunEnded` once a
`lineup`, `waivers` or `trade` run ends `done`: a goroutine reads the job's artifact through
`Host.ReadArtifact` (the same chat id and artifact name the UI reads) and calls `Host.Decide` one
point at a time, each call capped at 10s. Nothing waits on it, and no answer, error or timeout
changes an artifact or a dispatch. quack namespaces each as `ext:sleeper/<name>`. Each request
names its job chat (`DecideRequest.ChatID`), so quack records the decision in that chat's ledger
though `RunEnded` has no run context.

| Point | Question (primary) | Asked per | Baseline |
| --- | --- | --- | --- |
| `lineup_change` | `swap` (noul): should the proposed player start in this slot instead of the current one? | lineup starter row with `replaces` | `true` |
| `waiver_pickup` | `pickup` (noul): is this add worth its drop and the priority or FAAB? | `candidates` row; `also_checked` row | `true`; `false` |
| `waiver_priority` | `priority` (score, levels `0`-`4`: fifth or later ... first claim) | ranked `candidates` row, when 2+ are ranked | `5 - min(rank, 5)`: rank 1 is `4` |
| `trade_accept` | `accept` (noul): should this trade happen exactly as written, from my side? | `offers` row | `true` for `send`; `false` for `decline` and `counter` |

No point is restrictive. Option sets are static, as the declaration requires, so start/sit is
asked per recommended swap rather than as a choice over player ids, and lineup has no negatives.

States are built from facts, never from the analyst's argued conclusion, so Clef judges the
inputs instead of copying the agent it shadows. Left out: every `verdict`, `replaces`,
`confidence` and `rank`, and every piece of prose that argues for them (`why`, `delta`, the
scout's drop text, summaries, notes). In: the proposal being judged and the raw inputs. The lineup
state names the two players `current` and `proposed` instead of starter and bench. Each player row
is `{id, name, pos, team, opp, bye, status, practice, proj, floor, ceiling, recent}`: `opp` and
`bye` are joined from the schedule only for the live regular-season week, and `recent` is up to
three prior weeks of `pts_ppr` from the week stats, weeks with no stat line left out. `floor` and
`ceiling` are the analyst's own range estimate, the one opinion a row still carries. A waiver
state's `drop` is a player row: the players dump's facts when the scout's drop text resolves to
exactly one player through the name index, else only the name cut from that text. The trade state
carries the offer's `give`/`get` rows and `offered_by` (`me` or `partner`; the analyst's own
counter is `me`, since I would send it), not the full rosters. `give` is always what my team
sends: the trade schema defines both lists from my side, and an offer written from the offering
partner's side (its `give` on the partner's roster and none on mine) is flipped before it is asked.

Join keys, all in the state: `chat_id`, `league_id`, `season`, `week` (the stop's week, even when
the artifact names another; a trade uses the live NFL week) and

- `lineup_change`: `slot`, `current.id`, `proposed.id`. Score with the league's own
  `Matchups(league, week)` `players_points`: the swap was right when `proposed` outscored
  `current`.
- `waiver_pickup` / `waiver_priority`: `add.id`, plus `drop.id` when the drop resolved, else
  `drop.name`. Score the add's weekly points (`WeekStats` `pts_ppr`, or `players_points` once
  rostered) for weeks after `week` against the drop's.
- `trade_accept`: `partner_id`, `offer_index`, `give[].id`, `get[].id`; score rest-of-season
  points from `week` on.

A re-run of a job re-asks every row of its latest artifact, so the scorer keeps one decision per
key (the latest). Enable them in quack's `decisions.points` as
`"ext:sleeper/<name>": {enabled: true, handler: <handler>, mode: observe}`.

## The spec is the source of truth

Sleeper publishes no OpenAPI or Swagger document. `openapi.yaml` is owned by this module,
written from endpoints probed live on 2026-09-18 and response shapes derived from real
recorded data - not from Sleeper's prose docs. Paths marked `x-undocumented: true` were found
by probing (not in Sleeper's own docs) and may disappear without notice; treat a 404 from one
of those as expected, not a bug.

The generated client lives in `sleepergen/` (`go generate ./sleeper/...`, config
`sleepergen/genconfig.yaml`, pinned to `oapi-codegen/v2@v2.7.0` - the same version and pattern
quack uses for its Langfuse client in `internal/langfuse`). Client + models only, no server:
this module only ever calls out to `api.sleeper.app`.

Contract tests (`contract_test.go`) decode every fixture in `testdata/get/` into its generated
type with unknown fields disallowed - an extra or renamed field fails the test instead of a
404/400 in prod. Missing-field drift needs a second check: `DisallowUnknownFields` doesn't
catch a field that just disappears (it decodes to a zero value, silently), so `openapi.yaml`
marks the fields tools depend on `required`, and the same test also asserts every fixture
instance carries those keys, read straight from `openapi.yaml` (not a hardcoded copy) so the
spec stays the source of truth. `TestMutationDroppedRequiredFieldFails` proves the check
actually fires. `TestAllFixturesCovered` fails if a fixture has no case exercising it, so a
stale recording can't rot unnoticed.

## Fixtures

Recorded from the live API for two leagues in owner jffagerberg's (user_id
860211057418018816) chain:

- **Current season**: league `1356407594683482112` (2026, week 2) - league settings, rosters,
  users, matchups for weeks 1-2, transactions for weeks 1-2, both playoff brackets, traded
  picks, the league's drafts, one draft with its picks, NFL state, trending add/drop, one
  player, the players dump, week-2 projections and stats, one team's depth chart, the season
  schedule, week-2 scores (kickoff instants and venues), player research, and one player's
  season stats.
- **Past season**: league `1257102049204514816` (2025, complete) - league settings, rosters,
  winners bracket, matchups for weeks 1 and 14, transactions for weeks 1 and 5 (week 5 has two
  real trades - the only trade shape verified against live data; this league runs with
  `settings.pick_trading: 0`, so `Transaction.draft_picks` and both `traded_picks` endpoints
  are always `[]` and `TradedPick`'s field shape is unverified in both leagues), and its draft
  (`1257102049204514817`, all 140 picks) with its traded picks - recorded for the
  league-history job's `Chain` walk (`previous_league_id`), which the current league's chain
  reaches in one hop (a second hop exists live but isn't fixtured; `Chain` errors rather than
  silently truncating when it hits that gap - see `client.go`).

**Trimmed, not full dumps** (large upstream payloads, kept small deliberately):

- Players dump (`/v1/players/nfl`, ~14 MB live): trimmed to the union of every player_id
  referenced by the other fixtures - both leagues' rosters/starters, matchups, draft picks,
  transactions (adds/drops), trending add/drop, and the depth-chart sample (312 entries,
  including every DEF unit those fixtures cite). `TestPlayersDumpCoversReferencedIDs`
  (`client_test.go`) re-derives that reference set from the fixtures at test time and fails if
  the dump falls behind it again.
- Week-2 projections: trimmed to the current league's 143 rostered player_ids plus the top 30
  unrostered free agents by `pts_ppr` projection (173 entries). Week-2 stats: the live
  endpoint itself only carried 141 players total this early in the week (most hadn't accrued
  a stat line yet); intersected with the same keep-set, 11 remain.
- 2025 season stats: trimmed to the 140 players drafted in the past league.
- Week-2 scores: each game's `metadata` trimmed to `date_time`, `status`, `stadium_details`,
  `home_team`, and `away_team` (the live response also carries ~60 odds, forecast, and
  live-score keys).

Every other fixture is the live response as-is (all under 60 KB).

## Re-recording

```bash
cd sleeper
go run ./cmd/qa-mock --fixtures testdata/get --addr :8092 --record &
curl http://localhost:8092/v1/league/1356407594683482112   # any path from openapi.yaml
kill %1
```

`--record` proxies a fixture miss to the real `https://api.sleeper.app` and saves the
response under `testdata/get/<hash>.json`, keyed the same way `github/cmd/qa-mock` keys GitHub
fixtures: `sha256("GET_" + path [+ "_" + query])[:8 bytes hex] + ".json"`. Re-recording the
players dump, week-2 projections/stats, or the 2025 season stats will pull the full untrimmed
payload back down - re-run the trim script in this PR's history (or re-derive: filter to
rostered/drafted player_ids plus top free agents by `pts_ppr`) before committing. Re-recording
week-2 scores likewise needs its `metadata` trimmed back to the five keys listed above.

`cmd/qa-mock` with no `--record` flag (there is no `serve` subcommand - just the one binary and
flags above) replays only what's already recorded and 404s on a miss, so CI and
`client_test.go` never touch the real API.

## Client

`client.go` wraps the generated client with an in-memory cache (per-endpoint TTLs: state 5m,
league/rosters/users 10m, matchups and schedule 2m, players dump 24h, projections/stats 1h,
week scores 6h) and a player name index (lowercased "first last", last name, and DEF team codes
-> player_ids), built from whatever `PlayersDump` last fetched. `WeekScores` (the undocumented
`/scores/nfl/regular/{season}/{week}`) is read only for each game's kickoff (`start_time`) and
stadium; `weekSlate` (`tools_league.go`) joins it to the schedule on `game_id` and computes
`locked` from kickoff vs. now on every call, so no TTL delays a lock. A `flex-schedule` game's
`start_time` is a placeholder and is reported as `kickoff_tbd`; postponed and canceled games get
no kickoff. `Chain` walks a league's `previous_league_id` back through past seasons (newest
first, bounded to 10 seasons, cached 24h) for the league-history job.

`sleeper_schedule` returns that slate. `sleeper_roster`, `sleeper_matchup`, and `sleeper_player`
attach each player's own game from it by NFL team code (a DEF's player_id is its team), or
`bye`/`no_game_reason`, and only for the live regular-season week: the players dump holds
today's teams, so a past week is never joined. A schedule failure degrades to
`no_game_reason: "schedule unavailable"` instead of failing the tool. Reserve and taxi players
carry no game.

`kickoff_local` and `fetched_at_local` render in quack's configured zone (`Host.Location`), else
the process zone. The optional `timezone` config (an IANA name such as `America/Chicago`)
overrides quack's zone for this extension, and a tool's `tz` argument overrides both. All three
accept only region-style names (or `UTC`): `EST`/`CDT` load as fixed offsets that are an hour off
in DST. A bad `timezone` fails startup, a bad quack zone is ignored with a warning (process zone),
and a bad `tz` falls back with a `tz_note`. `time/tzdata` is embedded so zones resolve in images
without a zone database.

`NewClient(baseURL, httpClient)` takes an explicit base URL so tests (and later, tools) point
at `cmd/qa-mock` instead of the real API. Sleeper answers an unknown id with HTTP 200 and a
literal `null` body rather than a 404; `okJSON` treats that the same as a real not-found error
instead of returning a zero-valued struct with no error.
