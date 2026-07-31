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
  handles file uploads into the pool (`POST docs` multipart) + serving a file's bytes back
  (`GET docs/{id}/raw`), accepting only the kinds aigentic can read (images/PDF/text) and rejecting
  the rest with a reason. `ask.go` is the ONE server-side room-AI access point: it assembles the
  grounding from the whole pool (text inline; files base64 with their media type) and forwards the
  turn to aigentic on the caller's behalf (`POST ask`).
- `backend/internal/aigentic/` — presentr's thin client for aigentic's internal M2M `run` endpoint
  (shared-secret auth, subject resolved to live rights), mirroring hosuto's peer client. Disabled
  (no URL/secret) leaves the assistant reporting "not configured" rather than failing the daemon.
- `backend/internal/store/` — presentr's data pools. `atomic.go` holds the ONE atomic-write
  primitive + the shared `pool` base every pool reuses (temp→fsync→rename, one mutex,
  rollback-on-failed-save). `docs.go`/`chat.go`/`diagram.go` are the passive pools; each reaches
  the api via one access point. A document is EITHER typed text (its markdown inline in `Content`)
  or an uploaded file (its raw bytes kept out of band via `AddFile`/`Bytes`, so `List`/`Get` stay
  metadata-only — Portionierte Daten). The document pool is fronted by the `DocStore` interface
  (`docstore.go`), with two backends chosen at build time: the pure-Go JSON pool
  (`docstore_json.go`, default) and scheme (`docstore_scheme.go`, `//go:build scheme`); both carry
  the file bytes.
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

<!-- BEGIN HOLISTIC AXIOMS — generated by Mercury, do not edit -->
# Holistic — Axiome & Implementierungsregeln

_Automatisch aus Mercury ausgerollt. Nicht direkt bearbeiten; ändere die Axiome in DevLab → Mercury._

## Axiome

### architecture / minimalism
- **Intuitiv by Design** — Das System ist intuitiv by Design: Die Oberfläche erklärt sich aus sich selbst. Hilfstexte, erläuternde Notizen und Tooltips entfallen; sie sind nur in seltenen, sachlich unvermeidbaren Ausnahmefällen zulässig.
- **Keine ähnlichen Geschwister** — Funktionen mit ähnlichem Zweck werden unter einem gemeinsamen Zugang zusammengeführt, statt als getrennte, ähnliche Geschwister nebeneinander zu bestehen. Dies gilt innerhalb einer Codebase wie service-übergreifend und folgt aus dem Minimalism-Maxim.
- **Keine Redundanz** — Keine Änderung darf Redundanz schaffen oder die Oberfläche aufblähen. Bestehende SDK-Bausteine werden refaktoriert und wiederverwendet, statt lokalen Sondercode zu duplizieren.
- **Portionierte Daten** — Daten werden nicht im Überfluss dargestellt, sondern strikt portioniert und bedarfsgerecht — nur so viel, wie im jeweiligen Kontext benötigt wird.

### architecture / passive-data-pools
- **Passive Speicher** — Ein Data Pool ist ein reiner, passiver Speicher: Er hält Daten, ohne sie zu deuten, zu bewerten oder eigene Logik auszuführen. Jede Auswertung findet außerhalb des Pools statt.

### architecture / reuse-before-build
- **Reuse before Build** — 1. Suche die benötigte Komponente zuerst im geteilten SDK. 2. Existiert eine ähnliche: erweitere die SDK-Komponente. 3. Existiert keine, wird sie aber domänenübergreifend gebraucht: baue sie im SDK. 4. Nur wenn sie hochspezifisch für genau diesen Service ist: baue sie lokal.

### architecture / single-source-of-truth
- **Atomare Zugriffe** — Jeder lesende und schreibende Datenzugriff erfolgt atomar — unteilbar und ohne beobachtbaren Zwischenzustand.
- **Zugangspunkt wiederverwenden** — Existiert für eine Entität bereits ein Zugangspunkt, ist er zwingend wiederzuverwenden. Parallele Datenpfade zur selben Entität werden niemals angelegt.

### architecture / uniformity
- **CLI** — Jeder Service stellt eine CLI bereit. Sie folgt in Syntax und Semantik strikt dem einheitlichen Holistic-CLI-Standard, sodass alle Service-CLIs sich gleich bedienen lassen.
- **Code-Struktur** — Aufbau, Layout, Namenskonventionen und Repository-Grundgerüst eines Service entsprechen exakt denen der übrigen Holistic-Services. Struktur ist service-übergreifend uniform.

### environment
- **Account-Löschung** — Der Account-Löschvorgang bündelt seine Optionen an einer Stelle und legt explizit fest, wie verbleibende Userdaten behandelt werden.
- **Mehrsprachigkeit** — Alle sprachlichen Inhalte werden zeitnah in sämtliche unterstützten Holistic-Sprachen übersetzt. Die Sprache ist im UI jederzeit wählbar.
- **Rechtelose Dienste verborgen** — Dienste, zu denen ein User keine Rechte besitzt, werden ihm im Dashboard nicht angezeigt.
- **Rechtsklick-Menüs** — Wo ein Kontextmenü sinnvoll ist, wird es bereitgestellt — mit zweckmäßigen, nicht überflüssigen Einträgen, im Einklang mit dem Minimalism-Maxim.
- **Service-übergreifende Tabs** — Tabs sind nicht an einen einzelnen Service gebunden: Mehrere Services können denselben Tab gemeinsam gestalten, und die Umgebung unterstützt dies.
- **Tastaturnavigation in Listenelementen** — Alle listenartigen UI-Elemente, die mehrere Objekte enthalten, müssen sämtliche gängigen Tastaturkürzel unterstützen – insbesondere Kombinationen mit Cmd/Strg.
- **Zustandserhalt beim Reload** — Aktualisiert der User eine Seite im Browser, ist der zuvor gewählte Zustand wiederherzustellen — derselbe Tab, dieselbe Ansicht und dieselbe Session (etwa ein Service-Tab oder eine Chat-Session). Der User arbeitet an derselben Stelle weiter und muss nicht erneut selbst dorthin navigieren.

### interfaces
- **Kennzeichnungspflicht für KI-Modellantworten** — Jede Antwort auf einen KI-Prompt ist mit dem verwendeten Modell zu kennzeichnen.
- **Konfiguration** — Jeder Service stellt seine Konfiguration als vollständige Schnittstelle bereit. Sämtliche Konfiguration und Einstellung — insbesondere durch Admins — erfolgt gebündelt im zentralen Dashboard in einem eigenen Tab, analog zur zentralen Rechteverwaltung, jedoch für Konfigurationen statt Rechte. Falsch eingeordnete Konfiguration wird umgehend dorthin umgeordnet. In den Service-Tabs steht die User-Experience im Zentrum; sie werden nicht mit Konfiguration überfrachtet.
- **Leistungsbeanspruchung** — Jeder Service stellt seine Leistungsbeanspruchung als vollständige Schnittstelle bereit — seinen Bedarf an Rechenzeit, Arbeitsspeicher und weiteren Betriebsressourcen. Die Beanspruchung wird gebündelt im zentralen Dashboard sichtbar, analog zur zentralen Rechteverwaltung, sodass Last und Kapazität service-übergreifend beurteilt werden können. Der Service meldet seinen Verbrauch; die Bewertung liegt außerhalb. In den Service-Tabs steht die User-Experience im Zentrum, nicht die Last-Telemetrie.
- **MCP** — Jeder Service stellt seine Fähigkeiten als vollständige MCP-Schnittstelle bereit — als Model-Context-Protocol-Server, über den Agenten die Funktionen des Service nutzen. Der Server ist einheitlich benannt und über die zentrale Infrastruktur adressierbar, analog zur zentralen Rechteverwaltung; seine Adresse liegt server-seitig und nie im Request, sodass kein manipulierter Aufruf einen Agenten auf einen fremden Host lenkt. Jede über MCP angebotene Fähigkeit ist durch das Rechtesystem gedeckt.
- **Rechte** — Jeder Service stellt seine Rechte als vollständige Schnittstelle bereit — als Rechte-Manifest für die zentrale Rechteverwaltung. Jedes Recht ist symmetrisch aufgebaut und eins zu eins durch eine Systemgruppe gedeckt; Feingranularität nur dort, wo sie fachlich zwingend geboten ist. Vergabe und Verwaltung erfolgen gebündelt in der zentralen Rechteverwaltung, nicht in den einzelnen Service-Tabs. So bleibt das Rechtesystem über alle Services einheitlich und symmetrisch.
- **Speichernutzung** — Jeder Service stellt seine Speichernutzung als vollständige Schnittstelle bereit — welche Daten er in welchem Umfang hält. Die Nutzung wird gebündelt im zentralen Dashboard sichtbar, analog zur zentralen Rechteverwaltung, sodass Belegung und Wachstum service-übergreifend nachvollziehbar sind. Der Service meldet nur; die Auswertung liegt außerhalb. In den Service-Tabs steht die User-Experience im Zentrum, nicht die Speicher-Telemetrie.
- **Tiefe Implementierung von KI-Optionen** — Bei nichttrivialen Aufgaben sollen Usern KI-Optionen zur Verfügung stehen. Dies umfasst die gesamte Holistic-Servicelandschaft. Als Standard gilt hier der "Ask AI" Button, der durch den aigentic Service routet .Solange KI einen generelleren Use Case erfüllt, soll dieser "Ask AI" Standard verwendet werden. Im Falle eines spezifischeren Einsatzes, darf von diesem abgewägt werden.

### lawbooks
- **Bewusst designt** — Ein axiomatisches System wird bewusst als Ganzes entworfen, nicht historisch gewachsen. Organische Evolution wird vermieden; die Struktur ist absichtsvoll gestaltet.
- **Keine Beispiele** — Ein axiomatisches System verzichtet auf Beispiele. Es enthält ausschließlich formale Definitionen, keine illustrativen Einzelfälle.
- **Konsistente Überschriften** — Die Benennung von Überschriften folgt einer einheitlichen Konvention. Gleichartige Abschnitte tragen gleichartige Titel; wechselnde Benennungsmuster oder Synonyme für dieselbe Rolle sind unzulässig.
- **Wissenschaftliche Formulierung** — Ein axiomatisches System ist wissenschaftlich präzise formuliert und liegt in einer englischen Fassung mit deutscher Übersetzung vor.

### mobile / native-parity
- **Gespiegelte UI-API** — Das native UI-Paket spiegelt ausschließlich die visuelle Komponenten-API des Web-UI-Pakets — gleiche Export- und Prop-Namen — und rendert sie über native Widgets. Es entsteht dabei keine zweite Implementierung von Logik, Daten oder Rechten.
- **Geteilter Core** — Daten, Logik, Rechte und Internationalisierung liegen plattformneutral im geteilten Core-Paket und werden von Web- und Native-Apps identisch konsumiert. Der Core enthält keine DOM- oder Browser-Abhängigkeit.
- **Nur Render-Schicht doppelt** — Reuse-before-Build bleibt gewahrt: Die einzige zulässige Dopplung ist die technisch unteilbare visuelle Render-Schicht (Web-DOM gegenüber nativen Widgets). Der Reuse- und SDK-Audit wertet diese Render-Spiegelung nicht als Duplizierung.
- **Token-Auth nativ** — Native Apps authentisieren sich token-basiert (Bearer) gegen den Origin des gekoppelten Servers; Same-Origin-Cookies und die CSRF-Doppelabgabe entfallen. Der native Locale-Provider spiegelt die Web-Locale-API, persistiert die Locale jedoch in gerätesicherem Speicher statt im Browser-Storage.

### mobile / per-app-distribution
- **Eigenständige Apps** — Jeder Service wird als eigenständige App ausgeliefert; die Bundle-ID folgt der umgekehrten Domain-Notation <org>.holistic.<serviceId>. Die Launcher-App ist zugleich App-Manager (<org>.holistic.launcher). Der native App-Name ist der statische Anzeige-Name des Service-Plugins und wird nicht pro Sprache lokalisiert; Internationalisierung gilt für die Laufzeit-Inhalte der App, nicht für ihren Namen.
- **Installation pro Gerät** — Der Installationszustand der Launcher-App ist rein client-seitig und gilt pro Gerät. Er ist nie account- oder servergebunden und wird nicht zwischen Geräten synchronisiert.
- **Katalog nach Rechten** — Der Service-Katalog des Launchers blendet Dienste aus, zu denen der gekoppelte User keine Rechte besitzt oder die nicht standardmäßig sichtbar sind. Die rechteunabhängige Installations-Wahrheit bleibt davon unberührt.
- **Kein Web-Tab-Modell** — Das Web-Tab-Modell — mehrere Services gestalten einen gemeinsamen Tab mit — bleibt für die Web-Oberfläche verbindlich. Auf Mobil bildet jede Service-App ausschließlich den eigenen Service-Beitrag ab; service-übergreifende Sichten verbleiben in der Web-Oberfläche oder werden im Launcher aggregiert. Service-übergreifende Navigation erfolgt nativ über OS-Deep-Links in <org>.holistic.<targetId>.
- **Zentrale Konfiguration** — Konfiguration wird auf Mobil zentral im Launcher/App-Manager gebündelt geführt (analog zur zentralen Rechteverwaltung im Web), nicht in den einzelnen Service-Apps. Falsch in einer Service-App platzierte Konfiguration wird dorthin umgeordnet.

### universality
- **CLAUDE.md gehört ins Repo** — Entwickler-orientierte Dateien wie CLAUDE.md sind stets Teil des Repos; sie tragen die verbindlichen Holistic-Axiome, die bei jeder Implementierung gelten. Dies ist die zulässige Ausnahme zur Instanz-Neutralität: Die geteilte Verfassung gehört ins Repo, Instanz-Spezifika nicht.
- **Keine Instanz-Spezifika** — Holistic ist kein persönliches Projekt, sondern durchgehend so gestaltet, dass jede Organisation es betreiben kann. Kein Artefakt in den Repos bezieht sich auf eine konkrete Instanz: keine persönlichen Daten, keine hartcodierten nutzer- oder maschinenspezifischen Pfade, keine instanz-eigenen Domains. Instanz-Spezifika leben ausschließlich in der Laufzeit-Konfiguration.

## Implementierungsregeln

### communication
- **Axiome und Regeln automatisch einpflegen** — Wenn durch den Tonfall des Users angeleitet wird, dass etwas ein generelles Axiom oder Implementierungsregel sein soll, dann diese automatisch hier in Mercury sinnvoll hineinhießen und der User über das Hinzufügen der Regel / des Axioms benachrichtigen. Desweiteren wird das neue Axiom automatisch einem vorhandenem oder neuem Lauf zugeordnet, und der User wird darüber benachrichtigt (und kann ggf. nachträglich nachjustieren)
- **Erklären nach Feynman** — Kommuniziere mit dem User auf seinem Niveau. Er hat ein gutes Grundverständnis von Softwareentwicklung, kennt aber nicht jede Feinheit und nicht jeden Fachbegriff — besonders nicht die spezifischen Begriffe einzelner Frameworks. Überflute ihn nicht mit unnötig technischen Formulierungen oder Detailwissen.  Erklärst du etwas, dann grundlegend, einfach verständlich und dennoch technisch korrekt. Stütze jede Erklärung auf eine eingängige Analogie, die den Kern greifbar macht (Feynman-Methode: ein Prinzip so erklären, dass es ohne Vorwissen einleuchtet).
- **user-antwort** — Zusammenfassungen/Antworten kann den Usern sind kurz und knapp und folgen klarer Struktur: Live & deployed; noch nicht gepusht; Antworten auf Fragen (falls User fragen stellt)

### environment
- **Passwordless sudo vorausgesetzt** — Passwordless sudo ist für Claude auf dem Server aktiviert; dies ist beim Implementieren generell vorauszusetzen

### process
- **Deploy-Disziplin** — Ein neuer Service, ein Feature oder ein Bugfix wird unmittelbar live deployt. Signalisiert der Tonfall des Users, dass der Stand behalten wird oder zum nächsten Feature/Bug übergegangen wird, erfolgt zusätzlich automatisch der Push auf main (mainpush). Dies ist eine konkrete Vorgehensweise WÄHREND der Implementierung, keine nachträgliche Prüfung.
- **Implementierung auf Englisch** — Implementiere ausschließlich in Englisch. Holistic ist multilingual, doch die Übersetzung in alle weiteren Sprachen erfolgt nachgelagert im Nightly Run; dies hält den Token-Verbrauch wirtschaftlich.
- **Klärungspflicht** — Stelle bei Architektur- und Designfragen sowie bei jeder Unklarheit umgehend Rückfragen an den User, auch ohne explizite Aufforderung im Prompt und insbesondere im Hinblick auf die strikte Einhaltung der Maximen.
- **Mehrere Anforderungen ohne Rückbestätigung umsetzen** — Übermittelt der Nutzer mehrere Anforderungen in separaten Prompts, sind diese unmittelbar umzusetzen, ohne zuvor eine erneute Bestätigung einzuholen.

### (allgemein)
- **Self-Healing** — Holistic ist self-healing: Werden beim Implementieren oder Testen Komplikationen, Fehler oder Bugs aufgedeckt, sind diese automatisch mitzufixen.
<!-- END HOLISTIC AXIOMS -->
