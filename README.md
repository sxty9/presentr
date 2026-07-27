# presentr

**The presentation-room tutor.** A holistic service that makes an unintuitive presentation
room self-explanatory. You pour everything known about the room into a **document pool**
(device manuals, wiring notes, the layout, free text); presentr derives a **connection
diagram** of the devices and how they wire together; and a **chat assistant** answers
questions about the room, grounded in the documents and the diagram.

A Go backend behind the holistic Caddy proxy plus a dashboard plugin built on the
**`@holisdk/ui`** SDK (consumed, never vendored).

```
Browser ── https://<host> (Caddy, same-origin) ─┐
  ├─ /                          → holistic SPA (bundles this plugin)
  ├─ /api/*                     → holistic backend        (127.0.0.1:8770)
  └─ /api/services/presentr/*   → presentrd (Go)          (127.0.0.1:8783)
```

- **Single sign-on:** the daemon validates the same holistic session (HS256 JWT in the
  `h_access` cookie, secret `/etc/holistic/jwt-secret`) — no separate login.
- **Roles = Linux (single source of truth):** admin = membership in the `sudo` group; the one
  fine-grained right, `hp_presentr_use`, is a Linux group `privleg` can grant per user.
- **Least privilege:** the daemon runs as an unprivileged system user, sandboxed by systemd,
  and escalates nothing.

## The three tabs

| Tab | What it is | Backed by |
|---|---|---|
| **Docs** | The room's document pool — add text (files next), read as Markdown, delete. | passive JSON pool today; **scheme** in production |
| **Connection** | The wiring diagram derived from the docs, editable by hand, restorable to the document-derived state. | **aigentic** (generation) + a service-local diagram store |
| **Chat** | An assistant that explains the room, grounded in the docs + diagram; each answer labelled with its model. | **aigentic** ("Ask AI" standard) |

## Status

This repository is built out in committed, always-runnable steps.

- **Done — all three tabs.**
  - *Docs:* add text, keyboard-navigable list, Markdown preview, delete — over a passive,
    atomic document pool.
  - *Chat:* the room assistant, every turn routed through aigentic, answers labelled with their
    model, conversation persisted per user (same session after a reload).
  - *Connection:* the wiring diagram — generate it from the documents via aigentic, or build it
    by hand (drag devices, click ports to connect, add/remove); the first manual edit flips it to
    "manually modified" and the document-derived state stays one click away (Restore).
- **Done — scheme backend:** the document pool is fronted by a `DocStore` interface with two
  build-time backends — the pure-Go JSON pool (default) and **scheme** (`-tags scheme`), which
  stores each document as a described file in a scheme tree via its cgo C ABI.
- **Next:** the remaining holistic interfaces (config / consumption / storage / MCP) and file
  upload in Docs. See `CLAUDE.md` and the in-repo tasks.

Everything builds, vets and tests; the UI typechecks against the SDK and passes the service lint
lockdown; each stage is covered by an end-to-end smoke test.

## Local development

```bash
# Backend
cd backend && go build ./... && go vet ./... && go test ./...

# UI plugin in the holistic dashboard (holistic as a sibling repo)
ln -sfn "$PWD/ui" ../holistic/frontend/external/presentr
( cd ../holistic/frontend && pnpm --filter @holistic/app dev )   # http://localhost:5173
```

UI imports are restricted to `@holisdk/ui` + `react` (enforced by holistic's `eslint.services.cjs`).

## Install as a service

```bash
sudo ./service setup     # build, wire systemd + Caddy, declare rights, link the plugin, rebuild the SPA
```

Other commands: `service build`, `service start|stop|restart`, `service status`,
`service update`, `service uninstall [--purge]`. See `./service help`.

## Layout

```
service                     single-file CLI: setup / build / lifecycle (Holistic-CLI standard)
permissions/presentr.json   rights manifest (drop-in for privleg)
backend/                    Go daemon (presentrd)
  cmd/presentrd/              entry point — listens on 127.0.0.1:8783
  internal/auth/              shared-JWT validation + live group/admin resolution + CSRF (reused verbatim)
  internal/rights/            the hp_presentr_use group this service declares
  internal/api/               HTTP routes under /api/services/presentr/ — the only policy layer
  internal/store/             passive data pools with atomic reads/writes (atomic.go + docs.go)
ui/                         @holisdk/ui plugin (linked into holistic/frontend/external/presentr)
  index.tsx                   default-exports the ServicePlugin; imports ./i18n
  Dashboard.tsx               gates on the right, renders the three tabs (active tab persisted in nav.path)
  tabs/                       DocsTab · ConnectionTab · ChatTab
  i18n.ts                     registerMessages() catalog (en-US; nightly adds the rest)
  types.ts                    the API response shapes
```

## License

MIT — see [LICENSE](LICENSE).
