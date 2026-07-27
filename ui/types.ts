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
