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
- `backend/internal/api/api.go` — the HTTP surface under `/api/services/presentr/`. This is the
  ONLY place policy lives: it authenticates, enforces the right, stamps identity/time onto
  records and reaches the passive pools through their single access points. Add routes here.
- `backend/internal/store/` — presentr's data pools. `atomic.go` holds the ONE atomic-write
  primitive + the shared `pool` base every pool reuses (temp→fsync→rename, one mutex,
  rollback-on-failed-save). `docs.go`/`chat.go`/`diagram.go` are the passive pools; each reaches
  the api via one access point. The document pool is fronted by the `DocStore` interface
  (`docstore.go`), with two backends chosen at build time: the pure-Go JSON pool
  (`docstore_json.go`, default) and scheme (`docstore_scheme.go`, `//go:build scheme`).
- `backend/internal/rights/` — the `hp_presentr_use` group constant; mirrors `permissions/presentr.json`.
- `ui/index.tsx` — default-exports the `ServicePlugin`; `id` MUST equal the manifest `service`.
  Imports `./i18n` for its registration side-effect.
- `ui/Dashboard.tsx` — the plugin root: gates on the right, then renders the three tabs. The
  active tab lives in `nav.path`, so a browser reload lands on the same tab (Zustandserhalt).
- `ui/tabs/` — one file per tab (`DocsTab`, `ConnectionTab`, `ChatTab`). Each renders **only**
  `@holisdk/ui`, resolves every string via `useT()`.
- `ui/i18n.ts` — the `registerMessages()` catalog (en-US; nightly adds the other locales). Owns
  the localized `service.presentr` sidebar label and all UI strings.

## Building blocks (consumed, not vendored)

- **aigentic** — the shared AI service. The Chat tab and the diagram generator route AI through
  it (the "Ask AI" standard); every answer is labelled with the model that produced it. From the
  UI: `apiFor('aigentic').post('run', { header: { kind }, data })`. From the backend (background
  regeneration on behalf of a user): `POST /api/services/aigentic/internal/run` with the shared
  internal secret. Degrades gracefully when aigentic is absent.
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
- **Drag & Drop für Dateisammlungen** — UI-Elemente, die eine Sammlung von Dateien und/oder Ordnern repräsentieren, müssen browserseitiges Drag & Drop unterstützen.
- **Mehrsprachigkeit** — Alle sprachlichen Inhalte werden zeitnah in sämtliche unterstützten Holistic-Sprachen übersetzt. Die Sprache ist im UI jederzeit wählbar.
- **Rechtelose Dienste verborgen** — Dienste, zu denen ein User keine Rechte besitzt, werden ihm im Dashboard nicht angezeigt.
- **Rechtsklick-Menüs** — Wo ein Kontextmenü sinnvoll ist, wird es bereitgestellt — mit zweckmäßigen, nicht überflüssigen Einträgen, im Einklang mit dem Minimalism-Maxim.
- **Service-übergreifende Tabs** — Tabs sind nicht an einen einzelnen Service gebunden: Mehrere Services können denselben Tab gemeinsam gestalten, und die Umgebung unterstützt dies.
- **Tastaturnavigation in Listenelementen** — Alle listenartigen UI-Elemente, die mehrere Objekte enthalten, müssen sämtliche gängigen Tastaturkürzel unterstützen – insbesondere Kombinationen mit Cmd/Strg.
- **Tiefe Implementierung von KI-Optionen** — Bei nichttrivialen Aufgaben sollen Usern KI-Optionen zur Verfügung stehen. Dies umfasst die gesamte Holistic-Servicelandschaft. Als Standard gilt hier der "Ask AI" Button, der durch den aigentic Service routet .Solange KI einen generelleren Use Case erfüllt, soll dieser "Ask AI" Standard verwendet werden. Im Falle eines spezifischeren Einsatzes, darf von diesem abgewägt werden.
- **Zustandserhalt beim Reload** — Aktualisiert der User eine Seite im Browser, ist der zuvor gewählte Zustand wiederherzustellen — derselbe Tab, dieselbe Ansicht und dieselbe Session (etwa ein Service-Tab oder eine Chat-Session). Der User arbeitet an derselben Stelle weiter und muss nicht erneut selbst dorthin navigieren.

### interfaces
- **KI-Token-Verbrauch** — Jeder Dienst stellt KI-Token-Verbrauch ausschließlich über definierte Schnittstellen bereit. Dies ermöglicht zentrale Kontrolle des Token-Verbrauchs sowie systematische Datenanalyse.
- **Konfiguration** — Jeder Service stellt seine Konfiguration als vollständige Schnittstelle bereit. Sämtliche Konfiguration und Einstellung — insbesondere durch Admins — erfolgt gebündelt im zentralen Dashboard in einem eigenen Tab, analog zur zentralen Rechteverwaltung, jedoch für Konfigurationen statt Rechte. Falsch eingeordnete Konfiguration wird umgehend dorthin umgeordnet. In den Service-Tabs steht die User-Experience im Zentrum; sie werden nicht mit Konfiguration überfrachtet.
- **Leistungsbeanspruchung** — Jeder Service stellt seine Leistungsbeanspruchung als vollständige Schnittstelle bereit — seinen Bedarf an Rechenzeit, Arbeitsspeicher und weiteren Betriebsressourcen. Die Beanspruchung wird gebündelt im zentralen Dashboard sichtbar, analog zur zentralen Rechteverwaltung, sodass Last und Kapazität service-übergreifend beurteilt werden können. Der Service meldet seinen Verbrauch; die Bewertung liegt außerhalb. In den Service-Tabs steht die User-Experience im Zentrum, nicht die Last-Telemetrie.
- **MCP** — Jeder Service stellt seine Fähigkeiten als vollständige MCP-Schnittstelle bereit — als Model-Context-Protocol-Server, über den Agenten die Funktionen des Service nutzen. Der Server ist einheitlich benannt und über die zentrale Infrastruktur adressierbar, analog zur zentralen Rechteverwaltung; seine Adresse liegt server-seitig und nie im Request, sodass kein manipulierter Aufruf einen Agenten auf einen fremden Host lenkt. Jede über MCP angebotene Fähigkeit ist durch das Rechtesystem gedeckt.
- **Rechte** — Jeder Service stellt seine Rechte als vollständige Schnittstelle bereit — als Rechte-Manifest für die zentrale Rechteverwaltung. Jedes Recht ist symmetrisch aufgebaut und eins zu eins durch eine Systemgruppe gedeckt; Feingranularität nur dort, wo sie fachlich zwingend geboten ist. Vergabe und Verwaltung erfolgen gebündelt in der zentralen Rechteverwaltung, nicht in den einzelnen Service-Tabs. So bleibt das Rechtesystem über alle Services einheitlich und symmetrisch.
- **Speichernutzung** — Jeder Service stellt seine Speichernutzung als vollständige Schnittstelle bereit — welche Daten er in welchem Umfang hält. Die Nutzung wird gebündelt im zentralen Dashboard sichtbar, analog zur zentralen Rechteverwaltung, sodass Belegung und Wachstum service-übergreifend nachvollziehbar sind. Der Service meldet nur; die Auswertung liegt außerhalb. In den Service-Tabs steht die User-Experience im Zentrum, nicht die Speicher-Telemetrie.

### (allgemein)
- **Kennzeichnungspflicht für KI-Modellantworten** — Jede Antwort auf einen KI-Prompt ist mit dem verwendeten Modell zu kennzeichnen.

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

### pipelines
- **Kein stummes Ausbleiben** — Jeder Schritt einer Auslieferungskette trägt zu jedem Zeitpunkt genau einen belegten Zustand: ausgeführt, gescheitert, oder nicht anwendbar. Der Zustand „nicht anwendbar" liegt ausschließlich dann vor, wenn er aus einer nachgewiesenen Eigenschaft des Gegenstands folgt; das Fehlen einer Einrichtung ist keine solche Eigenschaft, sondern ein Mangel.  Der Zustand „ausgeführt" trifft ausschließlich auf einen tatsächlich ausgeführten Schritt zu. Eine umgesetzte Änderung ohne zugehörige Auslieferung ist ein Fehlschlag und als solcher ausgewiesen.  Eine Auslieferungskette enthält keinen Schritt, der durch eine Einstellung dauerhaft abgeschaltet ist. Was nicht gilt, ist nicht Teil der Kette.

### repositories
- **Gepflegte Branches** — Die Branches eines Repositories bleiben jederzeit geordnet und gepflegt. Zu unterscheiden sind zwei Arten: Ein VORGANGS-Branch trägt die Arbeit eines einzelnen Vorhabens und endet mit dessen Zusammenführung; ein BESTANDS-Branch hält dauerhaft einen Arbeitsstand und endet nie.  Für Vorgangs-Branches gilt: Die Benennung folgt der Form <Art>/<Beschreibung>, wobei die Art `fix` für die Behebung eines Mangels und `feature` für einen neuen Dienst oder eine neue Fähigkeit steht und die Beschreibung den Gegenstand in Kleinschreibung benennt. Ist die Arbeit im Standard-Branch angekommen, wird der Branch entfernt; ebenso ein Branch ohne offenen Vorgang und ohne eigenständige Arbeit. Tote Vorgangs-Branches verbleiben nicht im Repository.  Für Bestands-Branches gilt: Es gibt je Zweck höchstens einen, sein Name ist über alle Repositories gleich und benennt seinen Zweck. Er unterliegt nicht der Vorgangs-Konvention, weil er kein Vorhaben abbildet, sondern einen fortlaufenden Stand.  Die Menge der Vorgangs-Branches bildet zu jedem Zeitpunkt genau die Vorhaben ab, an denen tatsächlich gearbeitet wird.
- **Gleicher Schutz für jedes Repository** — Jedes Repository der Instanz unterliegt denselben Bedingungen, und zwar von seiner Anlage an, nicht nachträglich. Der Standard-Branch ist geschützt: Änderungen erreichen ihn ausschließlich über Pull Requests; der Schutz gilt unabhängig von den Rechten des Handelnden; das Überschreiben und das Löschen der Historie sind ausgeschlossen.  Für die Zusammenführung steht genau ein Verfahren zur Verfügung, und zwar dasjenige, das die Kennungen der zusammengeführten Commits unverändert erhält. Verfahren, die Commits unter neuer Kennung neu schreiben, sind ausgeschlossen, weil sie die Zuordnung zwischen dem fortlaufenden Arbeitsstand und dem Standard-Branch aufheben.  Die Anlage eines Repositories ohne diese Bedingungen ist unzulässig; sie werden im selben Vorgang gesetzt, der das Repository erzeugt. Ein Repository, dessen Bedingungen später abweichen, wird ohne Zutun in den vorgeschriebenen Zustand zurückversetzt, und die Abweichung wird festgehalten.

### universality
- **CLAUDE.md gehört ins Repo** — Entwickler-orientierte Dateien wie CLAUDE.md sind stets Teil des Repos. Sie führen die verbindlichen Holistic-Axiome nicht im Wortlaut, sondern verweisen auf den Bestand, in dem sie geführt werden; der Verweis benennt die Rolle dieses Bestands, niemals seine Adresse, die allein in der Laufzeit-Konfiguration liegt. Der verbindliche Wortlaut erreicht jede Implementierung über den Prompt, der sie beauftragt — damit gilt stets der aktuelle Stand, unabhängig vom Alter einer Datei im Repo. Dass diese Dateien überhaupt im Repo liegen, ist die zulässige Ausnahme zur Instanz-Neutralität: Der Verweis auf die geteilte Verfassung gehört ins Repo, Instanz-Spezifika nicht.
- **Keine Instanz-Spezifika** — Holistic ist kein persönliches Projekt, sondern durchgehend so gestaltet, dass jede Organisation es betreiben kann. Kein Artefakt in den Repos bezieht sich auf eine konkrete Instanz: keine persönlichen Daten, keine hartcodierten nutzer- oder maschinenspezifischen Pfade, keine instanz-eigenen Domains. Instanz-Spezifika leben ausschließlich in der Laufzeit-Konfiguration.

## Implementierungsregeln

### communication
- **Agenda** — Schreibt der User „Agenda", gibt die Antwort ausschließlich den aktuellen Stand wieder: was abgeschlossen ist, sofern zu diesem Zeitpunkt etwas abgeschlossen wurde; was gerade läuft oder worauf gewartet wird; und was offen, blockiert oder rückfragebedürftig ist. Es wird dabei nichts Neues begonnen und nichts anderes beantwortet. Die Ausgabe folgt der für Antworten an den User festgelegten Gliederung.
- **Axiome und Regeln automatisch einpflegen** — Wenn durch den Tonfall des Users angeleitet wird, dass etwas ein generelles Axiom oder Implementierungsregel sein soll, dann diese automatisch hier in Mercury sinnvoll hineinhießen und der User über das Hinzufügen der Regel / des Axioms benachrichtigen. Desweiteren wird das neue Axiom automatisch einem vorhandenem oder neuem Lauf zugeordnet, und der User wird darüber benachrichtigt (und kann ggf. nachträglich nachjustieren)
- **User-Antwort** — Jede Antwort an den User folgt derselben Gliederung aus drei fettgedruckten Überschriften: DONE für das, was abgeschlossen ist und jetzt funktioniert; IN PROGRESS für das, was noch läuft oder worauf gewartet wird, insbesondere bei mehreren gleichzeitig arbeitenden Agenten; STILL OPEN für Offenes, Blockiertes und ausstehende Rückfragen. Unter den Überschriften steht kein Fließtext, sondern eine tabellarische Aufstellung. Trifft eine Kategorie nicht zu, bleibt sie leer und wird nicht durch Fließtext ersetzt. Die Antwort bleibt knapp; Erklärungen gehören in die jeweilige Zeile, nicht in einen Vor- oder Nachspann.
- **Verständlich und eindeutig formulieren** — Kommuniziere auf dem Niveau eines erfahrenen Softwareentwicklers, der die Feinheiten einzelner Werkzeuge und Frameworks nicht kennt. Erkläre grundlegend, einfach verständlich und dennoch fachlich korrekt.  Eine Erklärung ist so kurz wie möglich und beginnt beim konkreten Fall — an einem Beispiel, an tatsächlichen Werten, an dem, was der User sieht. Einordnung, Vorgeschichte, Vergleichstabellen und Aufzählungen der Möglichkeiten entfallen, solange die Frage ohne sie beantwortet ist. Ein Bild wird nur verwendet, wenn es die Erklärung ERSETZT; mehrere Bilder nebeneinander sind unzulässig. Bleibt eine Erklärung unverstanden, wird sie durch eine einfachere ersetzt und nicht um eine weitere Ebene ergänzt.  Darüber hinaus gilt für jede Formulierung: 1. Eindeutigkeit vor Kürze: Jede Sache wird bei ihrem Namen genannt — eine Aufgabe, ein Vorgang, eine Datei, ein Zweig. Rückverweise wie „das neue", „das von vorhin" oder „der Lauf" sind unzulässig, wenn mehrere in Frage kommen. 2. Kein unerklärter Fachbegriff: Ein Begriff aus der Innensicht des Systems wird bei seiner ersten Nennung in einem Halbsatz erklärt oder durch ein gewöhnliches Wort ersetzt. Ein selbst erfundenes Ersatzwort ist ebenfalls ein Fachbegriff und daher unzulässig. 3. Kein Befund ohne Bedeutung: Zu jeder technischen Feststellung gehört, was daraus für den User folgt — was er sieht, was ausbleibt, was er tun kann. 4. Bildliche Wendungen werden aufgelöst oder weggelassen; ein Bild ersetzt keine Erklärung. 5. Laufen mehrere Vorgänge gleichzeitig, wird jeder einzeln benannt und sein Zustand einzeln genannt. Eine Sammelaussage über „die Vorgänge" tritt nicht an ihre Stelle. 6. Wird viel gleichzeitig bearbeitet, ordnet die Antwort zuerst zu, worum es geht, bevor sie berichtet, was geschah. 7. Eine Ursache wird nie als Sammelaussage benannt („manchmal geht es nicht", „aus verschiedenen Gründen"). Genannt wird die vollständige Liste der konkreten Fälle, jeder mit der Stelle, an der er entschieden wird, und der Unterscheidung, ob er einen Versuch verhindert oder einen Versuch scheitern lässt. Ist die Liste unvollständig, wird das gesagt.

### environment
- **HSL – Holistic Services Landscape** — HSL (Holistic Services Landscape) bezeichnet die Gesamtheit aus der Holistic-Instanz und allen Diensten, die Bestandteil des Holistic Dashboards sind.
- **Passwordless sudo vorausgesetzt** — Passwordless sudo ist systemseitig für den Agenten aktiviert und bei der Implementierung generell vorauszusetzen.

### process
- **Browser-Aufgaben delegieren** — Sobald der User eine Aufgabe im Browser ausführen muss, generiere unmittelbar einen einsatzbereiten Prompt für Claude for Chrome, der die Aufgabe eigenständig übernimmt.
- **Erledigtes vor dem Start feststellen** — Vor dem Start eines Vorgangs wird festgestellt, welcher Teil seiner Arbeit bereits vorliegt und wie weit dieser Teil auf dem Weg zum Standard-Branch gekommen ist. Die Feststellung erfolgt aus dem tatsächlichen Zustand der Repositories — Arbeitsstand, Lieferungen, Pull Requests, Standard-Branch — und nicht aus einem Merkmal, das der Vorgang über sich selbst führt.  Unterschieden werden drei Fälle: nicht umgesetzt; umgesetzt und nicht ausgeliefert; ausgeliefert. Ein einzelnes Merkmal "offen" genügt nicht, weil es den zweiten Fall mit dem ersten verwechselt.  Liegt Arbeit bereits vor, wird sie nicht erneut erzeugt. Der Vorgang beschränkt sich auf den noch nicht zurückgelegten Teil des Weges und meldet das als solches.
- **Implementierung auf Englisch** — Implementiere ausschließlich in Englisch. Holistic ist multilingual, doch die Übersetzung in alle weiteren Sprachen erfolgt nachgelagert im Nightly Run; dies hält den Token-Verbrauch wirtschaftlich.
- **Implementierung über Mercury** — Jede KI-gestützte Implementierung läuft über Mercury, damit sie erfasst, nachvollziehbar und an die Auslieferungskette gebunden ist. Wird eine interaktive Sitzung geöffnet, um zu implementieren, so implementiert diese Sitzung nicht selbst: Sie legt die Aufgabe als konkretes ToDo an und führt es unmittelbar aus. Sind alle Ausführungsplätze belegt, wird ein laufender Vorgang zurückgestellt, um dem ToDo Platz zu machen. Die Ausführung liefert unmittelbar aus; die Zusammenführung in den Standard-Branch erfolgt über den regulären, geordneten Weg. So folgt auch die von Hand angestoßene Arbeit derselben Kette und ist vollständig erfasst.
- **Klärungspflicht** — Stelle bei Architektur- und Designfragen sowie bei jeder Unklarheit umgehend Rückfragen an den User, auch ohne explizite Aufforderung im Prompt und insbesondere im Hinblick auf die strikte Einhaltung der Maximen.
- **Mehrere Anforderungen ohne Rückbestätigung umsetzen** — Übermittelt der Nutzer mehrere Anforderungen in separaten Prompts, sind diese unmittelbar umzusetzen, ohne zuvor eine erneute Bestätigung einzuholen.
- **Mehrstufige Implementierung ohne Unterbrechung** — Mehrstufige Implementierungen werden ohne Rückfragen vollständig bis zur letzten Phase durchgeführt, sofern keine offenen Fragen bestehen. Die Überprüfung durch den Nutzer erfolgt erst nach Abschluss aller Phasen.

### (allgemein)
- **Self-Healing** — Holistic ist self-healing: Werden beim Implementieren oder Testen Komplikationen, Fehler oder Bugs aufgedeckt, sind diese automatisch mitzufixen.
<!-- END HOLISTIC AXIOMS -->
