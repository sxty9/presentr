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

// ── aigentic contract (we call the shared assistant via apiFor('aigentic')) ──────────────────
// A prizm envelope: the run endpoint takes { header:{kind}, data } and returns { header, data }.
// data on the way in is an AigenticRequest; on the way out an AigenticResult.
export interface AigenticRequest {
  prompt: string;
  inline?: { path: string; content: string; mediaType?: string }[];
  outputFormat?: string; // "text" | "markdown" | "json"
  model?: string;
}

export interface AigenticResult {
  output: string;
  engine?: string;
  model?: string;
}

export interface PrizmEnvelope<T> {
  header: { kind: string };
  data: T;
}

