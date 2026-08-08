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

