// Shapes returned by the backend under /api/services/presentr/.

export interface Info {
  service: string;
  version: string;
  user: string;
  isAdmin: boolean;
  canUse: boolean;
}

// One item in the room's document pool (backend internal/store.Document).
export interface Document {
  id: string;
  title: string;
  kind: string; // "text" | "file"
  mime: string;
  description: string;
  content: string;
  size: number;
  author: string;
  created: number;

  // Extraction — the text read once out of a file (kind "file") and kept beside it, so questions work
  // with the text, not the raw bytes. state drives what the UI shows; a file uploaded before this
  // feature carries an empty state.
  extractState?: 'pending' | 'reading' | 'ready' | 'failed' | '';
  extractSource?: string; // "text-layer" (read exactly, no AI) | "ai" (from images) | "mixed" (both)
  extractModel?: string; // the AI model that read it, when images were read (Kennzeichnungspflicht)
  extractEngine?: string;
  extractError?: string; // why the read failed (state "failed"); a retry can clear it
  extractSize?: number;

  // Two independent read tracks, shown separately (the task's decoupling):
  //   - The TEXT track: extractTextLayer marks that this file's text layer was read exactly and locally,
  //     the instant the read began — no AI wait. Shown as read immediately, even while images still read.
  //   - The IMAGE track: extractSectionsDone/Total count the embedded images read out of how many, and
  //     extractSectionLabel names the most recent one by page ("image on page 6"). A text-layer file whose
  //     images are still being read is already state "ready" (its text is usable) with done < total.
  //   - extractSectionPhase names the current sub-step of the image in progress: "compressing" (the
  //     picture is being downscaled) or "reading" (its compact form is at the vision AI), so the wait
  //     shows "Compressing image 3 of 11" then "Reading image on page 6", not one opaque counter.
  extractTextLayer?: boolean;
  extractSectionsDone?: number;
  extractSectionsTotal?: number;
  extractSectionLabel?: string;
  extractSectionPhase?: 'compressing' | 'reading' | '';
}

// GET docs/{id}/extract — a file's read state plus the text that was read, so the UI can show exactly
// what the assistant will draw on.
export interface ExtractResponse {
  state: string;
  source: string;
  model: string;
  engine: string;
  error: string;
  size: number;
  textLayer: boolean;
  sectionsDone: number;
  sectionsTotal: number;
  sectionLabel: string;
  sectionPhase: string;
  text: string;
}

// GET docs — the room's documents, newest first.
export interface DocsResponse {
  docs: Document[];
}

// POST docs (multipart) — the outcome of a file upload: what landed, and what was turned away with
// a reason (an unusable file is never silently stored).
export interface UploadResponse {
  ok: boolean;
  documents: Document[];
  rejected?: { name: string; reason: string }[];
}

// One turn in the room-assistant conversation (backend internal/store.Message). model/engine label
// an assistant turn with the aigentic model that produced it; empty on user turns.
export interface ChatMessage {
  role: 'user' | 'assistant';
  text: string;
  model?: string;
  engine?: string;
  created: number;
}

// GET/PUT chats — the caller's conversation with the room assistant.
export interface ChatHistory {
  messages: ChatMessage[];
}

// ── the room's AI (presentr's own backend grounds it and routes to aigentic) ──────────────────
// POST ask — presentr's backend assembles the grounding from the pool (text AND uploaded files) and
// runs the turn through aigentic on the caller's behalf, so the UI sends only the prompt and the
// requested answer shape. Every answer carries the model/engine that produced it
// (Kennzeichnungspflicht für KI-Modellantworten).
export interface AskRequest {
  prompt: string;
  outputFormat?: 'text' | 'markdown' | 'json';
}

export interface AskResult {
  output: string;
  engine?: string;
  model?: string;
}

// ── connection diagram (backend internal/store) ──────────────────────────────────────────────
export interface DiagramPort {
  id: string;
  name: string;
}
export interface DiagramNode {
  id: string;
  name: string;
  symbol: string;
  x: number;
  y: number;
  ports: DiagramPort[];
}
export interface DiagramEdge {
  id: string;
  from: string;
  fromPort: string;
  to: string;
  toPort: string;
  label?: string;
}
export interface DiagramGraph {
  nodes: DiagramNode[];
  edges: DiagramEdge[];
}
// GET/PUT diagram — the current graph plus which state it is in.
export interface DiagramView extends DiagramGraph {
  state: 'document' | 'manual';
  modified: boolean;
  sourceKey: string;
  generated: number;
}

