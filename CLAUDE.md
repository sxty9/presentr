# CLAUDE.md

**presentr** — the presentation-room tutor. A holistic service that turns a pile of raw
knowledge about a presentation room (device manuals, wiring notes, the room layout, free
text) into something anyone can use: a searchable document pool, an auto-derived connection
diagram of the devices and how they wire together, and an assistant that explains the room
grounded in both. A Go backend behind the holistic Caddy proxy plus a dashboard plugin built
on the **`@holisdk/ui`** SDK (consumed, never vendored).

```
Browser ── https://<host> (Caddy, same-origin) ─┐
  ├─ /                          → holistic SPA (bundles this plugin)
  ├─ /api/*                     → holistic backend        (127.0.0.1:8770)
  └─ /api/services/presentr/*   → presentrd (Go)          (127.0.0.1:8783)
```

The three tabs mirror the three workflow stages (and the sketches): **Docs** (the document
pool), **Connection** (the diagram derived from it), **Chat** (the assistant over both).

## Where things are

- `service` — the CLI. Auto-detects the service id from `permissions/presentr.json`; owns
  `setup`/lifecycle and generates the systemd unit, Caddy route and rights drop-in inline. It
  follows the uniform Holistic-CLI standard.
- `backend/internal/auth/auth.go` — shared-JWT (`h_access`) validation, live OS group/admin
  resolution, CSRF. Service-agnostic; taken verbatim from the template — reuse as-is.
- `backend/internal/api/` — the HTTP surface under `/api/services/presentr/`. This is the ONLY
  place policy lives: it authenticates, enforces the right, stamps identity/time onto records and
  reaches the passive pools through their single access points. Add routes in `api.go`. `files.go`
  handles file uploads into the pool (`POST docs` multipart, up to 100 MB per file — read as a STREAM
  and stored to disk as it arrives, never buffered whole) + streaming a file's bytes back
  (`GET docs/{id}/raw`, via `http.ServeContent` so a big file is not held in memory and range
  requests work), accepting only the kinds aigentic can read (images/PDF/text) and rejecting the
  rest — or an over-limit file — with a NAMED reason, never a torn stream. `ask.go` is the ONE
  server-side room-AI access point: it assembles the grounding from the whole pool (text inline;
  files base64 with their media type) bounded by aigentic's per-request ceiling, NAMES any document
  too large to include so the answer discloses what it could not read, and forwards the turn to
  aigentic on the caller's behalf (`POST ask`). Splitting a large document into question-relevant
  sections (RAG) is a capability aigentic does not yet have; presentr names the gap, it does not
  re-implement retrieval locally.
- `backend/internal/aigentic/` — presentr's thin client for aigentic's internal M2M `run` endpoint
  (shared-secret auth, subject resolved to live rights), mirroring hosuto's peer client. Disabled
  (no URL/secret) leaves the assistant reporting "not configured" rather than failing the daemon.
- `backend/internal/store/` — presentr's data pools. `atomic.go` holds the ONE atomic-write
  primitive + the shared `pool` base every pool reuses (temp→fsync→rename, one mutex,
  rollback-on-failed-save). `docs.go`/`chat.go`/`diagram.go` are the passive pools; each reaches
  the api via one access point. A document is EITHER typed text (its markdown inline in `Content`)
  or an uploaded file (its raw bytes kept out of band, STREAMED in via `AddFile` — a 100 MB upload
  never sits whole in memory — read back bounded via `Bytes` for the AI grounding or streamed via
  `OpenBlob` for download, so `List`/`Get` stay metadata-only — Portionierte Daten). The document
  pool is fronted by the `DocStore` interface (`docstore.go`), with two backends chosen at build
  time: the pure-Go JSON pool (`docstore_json.go`, default; streams file bytes straight to a blob
  file) and scheme (`docstore_scheme.go`, `//go:build scheme`; its FFI takes a whole buffer, so a
  large file transiently costs its size in memory there); both carry the file bytes.
- `backend/internal/rights/` — the `hp_presentr_use` group constant; mirrors `permissions/presentr.json`.
- `ui/index.tsx` — default-exports the `ServicePlugin`; `id` MUST equal the manifest `service`.
  Imports `./i18n` for its registration side-effect.
- `ui/Dashboard.tsx` — the plugin root: gates on the right, then renders the three tabs. The
  active tab lives in `nav.path`, so a browser reload lands on the same tab (Zustandserhalt).
- `ui/tabs/` — one file per tab (`DocsTab`, `ConnectionTab`, `ChatTab`). Each renders **only**
  `@holisdk/ui`, resolves every string via `useT()`.
- `ui/roomAI.ts` — the UI-side access point to the room's AI: a thin client of presentr's own
  `POST ask` endpoint. Both the Chat tab and the Connection diagram call `askRoom(api, …)` with only
  a prompt + output shape; the backend does the grounding (the pool's text AND uploaded files), so
  file bytes never round-trip out to the browser and back.
- `ui/i18n.ts` — the `registerMessages()` catalog (en-US; nightly adds the other locales). Owns
  the localized `service.presentr` sidebar label and all UI strings.

## Building blocks (consumed, not vendored)

- **aigentic** — the shared AI service. The room assistant and the diagram extraction route AI
  through it (the "Ask AI" standard); every answer is labelled with the model that produced it.
  presentr calls it FROM THE BACKEND on the caller's behalf — `POST
  /api/services/aigentic/internal/run` with the shared internal secret (`backend/internal/aigentic`)
  — because the grounding includes uploaded files whose bytes must reach aigentic (it reads images
  as vision, PDFs as documents, text inline). Wired via runtime config (`PRESENTR_AIGENTIC_URL` +
  `AIGENTIC_INTERNAL_SECRET[_FILE]`); degrades gracefully (assistant "not configured") when absent.
- **scheme** — the production backend for the document pool: a mutable, path-addressed document
  store (Rust), consumed in-process via a cgo binding to its C ABI, behind `//go:build scheme`
  (`docstore_scheme.go`). Each document is one described file at `documents/<id>` (scheme requires
  a description on every node). The default build is pure-Go (the JSON pool), so the service always
  builds and runs with no toolchain; scheme is a build-time choice (`-tags scheme`, CGO, the sibling
  `libscheme_ffi.a`) — mirroring how aigentic embeds scheme.

## Rules

- Enforce the right as `isAdmin || group ∈ user.groups`, in both the backend and the UI.
- Keep three things in sync: `permissions/presentr.json` ⇄ `internal/rights` ⇄ the UI right constant.
- No hardcoded UI strings: author them in `ui/i18n.ts` and resolve with `useT()` — the shared
  SDK i18n engine, never a local copy. Author in English; the nightly run translates.
- UI may import only `@holisdk/ui` and `react` (holistic's `eslint.services.cjs` enforces it).
- The daemon runs unprivileged and escalates nothing.
- A data pool stays passive (it stores and returns; it never filters, evaluates or applies
  policy — do that in the api layer) and is reached through one access point per entity. Every
  read and write is atomic: no partial state is ever observable, in memory or on disk.
- No instance-specifics in the repo (no hardcoded domains, users or machine paths). The dev/prod
  hosts live only in the runtime Caddy config, exactly like every other holistic service.

## Verify (from the repo root)

```bash
# Backend (default pure-Go pool)
(cd backend && go build ./... && go vet ./... && go test ./...)

# Backend with the scheme document backend (needs the sibling scheme repo + libscheme_ffi.a built)
(cd backend && CGO_ENABLED=1 go build -tags scheme ./...)

# Rights manifest
python3 ../holistic/services/dashboard/lib/holistic-perms.py validate ./permissions

# UI plugin typecheck (service dir has no node_modules — link react from a sibling, tsc in holisdk)
APP="$(cd ../holistic/frontend/app/node_modules && pwd)"
mkdir -p ui/node_modules/@types
ln -sfn "$APP/react" ui/node_modules/react
ln -sfn "$APP/react-dom" ui/node_modules/react-dom
ln -sfn "$APP/@types/react" ui/node_modules/@types/react
ln -sfn "$APP/@types/react-dom" ui/node_modules/@types/react-dom
../holisdk/node_modules/.bin/tsc -p ui/tsconfig.json --noEmit
```

<!-- holistic:constitution:begin -->
# Holistic — Verfassung

Für dieses Repository gelten die Holistic-Axiome und Implementierungsregeln.
Ihr verbindlicher Wortlaut wird nicht hier geführt, sondern mit jedem
Implementierungsauftrag mitgeliefert. So gilt immer der aktuelle Stand.

**Arbeitest du im Auftrag von Mercury:** Der Wortlaut steht vollständig in
deinem Prompt. Er hat Vorrang vor jeder anderen Fassung, die dir begegnet.

**Arbeitest du in einer von Hand geöffneten Sitzung:** Implementiere nicht
selbst. Lege die Arbeit als ToDo in Mercury an und führe es aus — dann kommt
der verbindliche Wortlaut auf dem regulären Weg. Der Verfassungs-Bestand wird
in der Laufzeit-Konfiguration der Instanz benannt.
<!-- holistic:constitution:end -->
