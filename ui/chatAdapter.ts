// presentr's ChatAdapter — how presentr drives the ONE shared <Chat> building block from
// @holisdk/ui, the SAME chat aigentic uses. There is no longer a bespoke presentr chat next to a
// richer aigentic one (the "similar siblings" the axioms forbid): the whole experience — many
// conversations with switch/create/delete, machine+model choice, model-tagged answers,
// reload-surviving history, wrapping prose — lives once in the shared chat, and this adapter is
// presentr's single source of truth for its data, so there is never a second data path to the same
// conversations.
//
// What presentr CONTRIBUTES stays presentr's: the answers are GROUNDED in the room's document pool.
// That grounding is assembled on presentr's BACKEND (POST ask) — uploaded PDFs/images are read
// there so their bytes never round-trip through the browser — so `send` here posts only the prompt +
// the chosen engine/model, and the backend adds the grounding and routes to aigentic on the caller's
// behalf. History persists via presentr's own GET/PUT chats (one opaque per-account blob, keyed by
// the server-stamped account), so it follows the user across devices and survives a restart.
import type {
  ChatAdapter,
  ChatEngine,
  ChatMessage,
  Conversation,
  NewConversation,
  SendInput,
  ServiceApiClient,
} from '@holisdk/ui';
import type { AskResult } from './types';

// The assistant's role, sent as prompt guidance (not a user-facing string — it is model input).
// This is presentr's OWN persona: an explainer for an unintuitive presentation room, kept grounded
// in the room documents the backend supplies as context.
const PREAMBLE =
  'You are presentr, the built-in assistant for a company presentation room. Your job is to make ' +
  'an unintuitive room self-explanatory: explain clearly and simply, as if to a colleague who has ' +
  'never used the room, and stay strictly grounded in the room documents provided as context. When ' +
  'the documents do not cover something, say so plainly instead of guessing. Prefer concrete, ' +
  'step-by-step guidance (which cable, which port, which button).';

// The machines presentr can route to, and the static Claude models — the SAME engine taxonomy
// aigentic exposes, because presentr routes every turn through aigentic (the "Ask AI" standard).
// This taxonomy is aigentic's, not the shared chat's: it belongs to whichever services consume
// aigentic, so presentr — a second consumer — mirrors it here rather than inventing a different one.
// The live per-machine model list comes from aigentic's own /models access point (see engines()).
const ENGINES = [
  { id: 'choose', label: 'Auto' },
  { id: 'ollama', label: 'Local' },
  { id: 'claude-cli', label: 'Claude CLI' },
  { id: 'claude-api', label: 'Claude API' },
] as const;
const CLAUDE_FALLBACK = [
  { id: 'claude-sonnet-4-6', label: 'Sonnet' },
  { id: 'claude-opus-4-8', label: 'Opus' },
  { id: 'claude-haiku-4-5-20251001', label: 'Haiku' },
];

// The catalog aigentic's /models returns: the static Claude list + the locally-pulled ollama models.
interface ModelCatalog {
  claude: { id: string; label: string }[];
  ollama: string[];
}

// A stored conversation. Mirrors the shared Conversation plus the messages themselves and the last
// machine/model used, so reopening a conversation restores its selection (Zustandserhalt). The
// backend treats the whole list as opaque JSON, so these fields are presentr's to define.
interface StoredMsg {
  role: 'user' | 'assistant';
  content: string;
  model?: string; // for an assistant turn: the model that produced it (Kennzeichnungspflicht)
}
interface StoredChat {
  id: string;
  title: string;
  updatedAt: number;
  engineId?: string;
  modelId?: string;
  messages: StoredMsg[];
}

function newId(): string {
  try {
    return crypto.randomUUID();
  } catch {
    return `c-${Date.now()}-${Math.floor(Math.random() * 1e9)}`;
  }
}

// titleOf derives a conversation's rail label from its first user message.
function titleOf(text: string): string {
  const t = text.trim().replace(/\s+/g, ' ');
  if (!t) return 'Room chat';
  return t.length > 48 ? `${t.slice(0, 48)}…` : t;
}

// migrate accepts whatever GET chats returns and yields the conversation list. It handles the
// pre-shared-chat shape too: presentr used to store a single conversation as a bare array of
// {role,text,...} messages — those are wrapped into one conversation so a user's earlier history is
// not lost when the shared chat takes over.
function migrate(raw: unknown): StoredChat[] {
  if (!Array.isArray(raw) || raw.length === 0) return [];
  const first = raw[0] as Record<string, unknown>;
  if (first && typeof first.text === 'string' && !('messages' in first)) {
    const messages: StoredMsg[] = (raw as Record<string, unknown>[])
      .filter((m) => m && (m.role === 'user' || m.role === 'assistant') && typeof m.text === 'string')
      .map((m) => ({ role: m.role as 'user' | 'assistant', content: String(m.text), model: typeof m.model === 'string' ? m.model : undefined }));
    if (!messages.length) return [];
    const firstUser = messages.find((m) => m.role === 'user');
    return [{ id: newId(), title: titleOf(firstUser?.content ?? 'Room chat'), updatedAt: Date.now(), messages }];
  }
  return raw as StoredChat[];
}

// makeRoomChatAdapter builds presentr's adapter over its own service api client. `apiFor` reaches
// aigentic's /models (the SAME access point aigentic's own picker uses) for the live machine/model
// catalog — never for a turn, which always goes through presentr's grounded POST ask.
export function makeRoomChatAdapter(api: ServiceApiClient, apiFor: (id: string) => ServiceApiClient): ChatAdapter {
  // The whole conversation list, loaded once then the single in-memory truth. Every mutation
  // persists the full list (an await'd PUT) before returning, so a reload always reflects it.
  let chats: StoredChat[] = [];
  let loaded: Promise<void> | null = null;
  let catalog: ModelCatalog | null = null;

  function ensureLoaded(): Promise<void> {
    if (!loaded) {
      loaded = api
        .get<unknown>('chats')
        .then((got) => {
          chats = migrate(got);
        })
        .catch(() => {
          chats = []; // no stored history / offline — start empty
        });
    }
    return loaded;
  }

  async function save(): Promise<void> {
    try {
      await api.put('chats', chats);
    } catch {
      // Best-effort: a failed write keeps the in-memory list; a later turn retries the whole list.
    }
  }

  const byId = (id: string) => chats.find((c) => c.id === id);
  const toConversation = (c: StoredChat): Conversation => ({ id: c.id, title: c.title, updatedAt: c.updatedAt, engineId: c.engineId, modelId: c.modelId });
  const toMessages = (c: StoredChat): ChatMessage[] => c.messages.map((m, i) => ({ id: `${c.id}:${i}`, role: m.role, content: m.content, model: m.model }));

  return {
    // The machines + models the user may pick from — built from aigentic's live catalog with the
    // SAME rules aigentic's own chat uses: Auto (choose) leads and carries one pseudo-model; the
    // Claude machines list the static models (or the fallback); the local machine lists whatever
    // ollama has pulled, and is omitted when it has none (nothing local to run). A missing catalog
    // (aigentic down, or the user lacks its run right) degrades to Auto + Claude, so model choice
    // still works.
    async engines(): Promise<ChatEngine[]> {
      if (!catalog) {
        catalog = await apiFor('aigentic')
          .get<ModelCatalog>('models')
          .catch(() => ({ claude: [], ollama: [] as string[] }));
      }
      const claude = (catalog.claude.length ? catalog.claude : CLAUDE_FALLBACK).map((m) => ({ id: m.id, label: m.label }));
      const ollama = catalog.ollama.map((m) => ({ id: m, label: m }));
      const out: ChatEngine[] = [];
      for (const e of ENGINES) {
        if (e.id === 'choose') out.push({ id: e.id, label: e.label, models: [{ id: '', label: 'Auto' }] });
        else if (e.id === 'ollama') {
          if (ollama.length) out.push({ id: e.id, label: e.label, models: ollama });
        } else out.push({ id: e.id, label: e.label, models: claude });
      }
      return out;
    },

    async listConversations(): Promise<Conversation[]> {
      await ensureLoaded();
      return [...chats].sort((a, b) => b.updatedAt - a.updatedAt).map(toConversation);
    },

    async loadMessages(conversationId: string): Promise<ChatMessage[]> {
      await ensureLoaded();
      const c = byId(conversationId);
      return c ? toMessages(c) : [];
    },

    async createConversation(init?: NewConversation): Promise<Conversation> {
      await ensureLoaded();
      const c: StoredChat = {
        id: newId(),
        title: init?.title || 'Room chat',
        updatedAt: Date.now(),
        engineId: init?.engineId,
        modelId: init?.modelId,
        messages: [],
      };
      chats.unshift(c);
      await save();
      return toConversation(c);
    },

    async deleteConversation(conversationId: string): Promise<void> {
      await ensureLoaded();
      chats = chats.filter((c) => c.id !== conversationId);
      await save();
    },

    // send runs one user turn. Multi-turn is stateless on the backend: the whole conversation is
    // re-sent as one transcript prompt (after presentr's persona preamble) so the model continues
    // after the trailing "Assistant:". The turn goes through presentr's POST ask, which grounds it
    // in the room's document pool and routes it to aigentic with the chosen engine/model.
    async send(input: SendInput): Promise<ChatMessage> {
      await ensureLoaded();
      const c = byId(input.conversationId);
      const prior: StoredMsg[] = c ? c.messages : [];
      const turns = [...prior, { role: 'user' as const, content: input.text }];
      const transcript = turns.map((m) => `${m.role === 'user' ? 'User' : 'Assistant'}: ${m.content}`).join('\n\n') + '\n\nAssistant:';
      const prompt = `${PREAMBLE}\n\n${transcript}`;

      const res = await api.post<AskResult>('ask', {
        prompt,
        outputFormat: 'markdown',
        engine: input.engineId,
        model: input.modelId,
      });
      const answer = (res.output || '').trim();
      const model = res.model || res.engine || undefined;

      if (c) {
        c.messages = [...prior, { role: 'user', content: input.text }, { role: 'assistant', content: answer, model }];
        c.updatedAt = Date.now();
        c.engineId = input.engineId;
        c.modelId = input.modelId;
        chats = [c, ...chats.filter((x) => x.id !== c.id)];
        await save();
      }
      const idx = c ? c.messages.length - 1 : 0;
      return { id: `${input.conversationId}:${idx}`, role: 'assistant', content: answer, model, createdAt: Date.now() };
    },
  };
}
