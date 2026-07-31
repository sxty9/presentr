import { useEffect, useRef, useState } from 'react';
import {
  Badge,
  Box,
  Button,
  EmptyState,
  IconButton,
  Markdown,
  Panel,
  SendIcon,
  Spinner,
  Stack,
  Text,
  Textarea,
  cn,
  useLiveQuery,
  useT,
  type ServiceContextProps,
} from '@holisdk/ui';
import type { ChatHistory, ChatMessage, DocsResponse } from '../types';
import { askRoom } from '../roomAI';

// The room assistant — the heart of presentr. The user asks questions and the assistant answers as
// an explainer, grounded in the document pool (text AND uploaded PDFs/images). Per the holistic
// "Ask AI" standard the AI runs in the shared aigentic service; presentr's backend grounds each turn
// in the pool and routes it there (POST ask), so this tab sends only the prompt and persists the
// transcript for reload continuity. Every assistant answer is labelled with the model that produced it.

// The assistant's role, sent as prompt guidance (not a user-facing string — it is model input).
const PREAMBLE =
  'You are presentr, the built-in assistant for a company presentation room. Your job is to make ' +
  'an unintuitive room self-explanatory: explain clearly and simply, as if to a colleague who has ' +
  'never used the room, and stay strictly grounded in the room documents provided as context. When ' +
  'the documents do not cover something, say so plainly instead of guessing. Prefer concrete, ' +
  'step-by-step guidance (which cable, which port, which button).';
const NO_DOCS_NOTE =
  'Note: the document pool is currently empty, so you have no room-specific context yet. Answer ' +
  'generally and suggest what information should be added to the Docs tab.';

export function ChatTab({ api, ui }: Pick<ServiceContextProps, 'api' | 'ui'>) {
  const t = useT();
  const docsQ = useLiveQuery<DocsResponse>(() => api.get<DocsResponse>('docs'), 15000);
  const [messages, setMessages] = useState<ChatMessage[] | null>(null); // null while loading
  const [input, setInput] = useState('');
  const [sending, setSending] = useState(false);
  const scrollRef = useRef<HTMLElement>(null);

  // Load the persisted conversation once (Zustandserhalt: same session after a browser reload).
  useEffect(() => {
    let alive = true;
    api
      .get<ChatHistory>('chats')
      .then((h) => alive && setMessages(h.messages ?? []))
      .catch(() => alive && setMessages([]));
    return () => {
      alive = false;
    };
  }, [api]);

  // Keep the latest turn in view.
  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages, sending]);

  async function send() {
    const q = input.trim();
    if (!q || sending || messages === null) return;
    const convo: ChatMessage[] = [...messages, { role: 'user', text: q, created: nowSec() }];
    setMessages(convo);
    setInput('');
    setSending(true);
    // Persist the question immediately so a reload mid-answer keeps it.
    void api.put('chats', { messages: convo }).catch(() => {});
    try {
      const docs = docsQ.data?.docs ?? [];
      const transcript = convo.map((m) => `${m.role === 'user' ? 'User' : 'Assistant'}: ${m.text}`).join('\n\n');
      const prompt = `${PREAMBLE}\n\n${docs.length === 0 ? NO_DOCS_NOTE + '\n\n' : ''}${transcript}\n\nAssistant:`;
      // The backend grounds the turn in the whole pool (text AND uploaded files) — the UI sends only
      // the prompt and the requested shape.
      const result = await askRoom(api, { prompt, outputFormat: 'markdown' });
      const answer: ChatMessage = {
        role: 'assistant',
        text: (result.output || '').trim() || t('presentr.chatEmpty'),
        model: result.model,
        engine: result.engine,
        created: nowSec(),
      };
      const next = [...convo, answer];
      setMessages(next);
      void api.put('chats', { messages: next }).catch(() => {});
    } catch (e) {
      ui.toast({ title: t('presentr.chatFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setSending(false);
    }
  }

  async function clearChat() {
    const ok = await ui.confirm({
      title: t('presentr.chatClearTitle'),
      description: t('presentr.chatClearConfirm'),
      danger: true,
      confirmLabel: t('presentr.chatClear'),
    });
    if (!ok) return;
    setMessages([]);
    void api.put('chats', { messages: [] }).catch(() => {});
  }

  const msgs = messages ?? [];

  return (
    <Stack gap={3}>
      <Stack direction="row" justify="between" align="center" gap={3}>
        <Stack gap={0}>
          <Text weight="semibold">{t('presentr.chatHeading')}</Text>
          <Text variant="caption" color="tertiary">
            {t('presentr.chatSubtitle')}
          </Text>
        </Stack>
        {msgs.length > 0 && (
          <Button variant="ghost" onClick={clearChat}>
            {t('presentr.chatClear')}
          </Button>
        )}
      </Stack>

      <Panel className="p-0">
        <Box ref={scrollRef} className="max-h-[52vh] overflow-y-auto p-4">
          {messages === null ? (
            <Stack align="center" justify="center" className="py-10">
              <Spinner />
            </Stack>
          ) : msgs.length === 0 ? (
            <EmptyState icon={<SendIcon />} title={t('presentr.chatEmptyTitle')} description={t('presentr.chatEmptyBody')} />
          ) : (
            <Stack gap={4}>
              {msgs.map((m, i) => (
                <Bubble key={i} m={m} />
              ))}
              {sending && (
                <Stack direction="row" align="center" gap={2}>
                  <Spinner />
                  <Text variant="footnote" color="secondary">
                    {t('presentr.chatThinking')}
                  </Text>
                </Stack>
              )}
            </Stack>
          )}
        </Box>
      </Panel>

      <Stack direction="row" gap={2} align="end">
        <Textarea
          className="flex-1"
          rows={2}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault();
              void send();
            }
          }}
          placeholder={t('presentr.chatPlaceholder')}
          disabled={sending}
        />
        <IconButton label={t('presentr.chatSend')} variant="primary" disabled={sending || !input.trim()} onClick={() => void send()}>
          <SendIcon />
        </IconButton>
      </Stack>
    </Stack>
  );
}

function Bubble({ m }: { m: ChatMessage }) {
  const isUser = m.role === 'user';
  return (
    <Stack align={isUser ? 'end' : 'start'} gap={1}>
      <Box className={cn('max-w-[85%] rounded-lg px-3 py-2', isUser ? 'bg-accent text-accent-fg' : 'bg-fill/10')}>
        {isUser ? <Text className="whitespace-pre-wrap">{m.text}</Text> : <Markdown text={m.text} />}
      </Box>
      {!isUser && (m.engine || m.model) && (
        <Stack direction="row" align="center" gap={2} className="px-1">
          <Badge variant="accent">{m.engine || 'ai'}</Badge>
          {m.model && (
            <Text variant="caption" color="tertiary">
              {m.model}
            </Text>
          )}
        </Stack>
      )}
    </Stack>
  );
}

function nowSec(): number {
  return Math.floor(Date.now() / 1000);
}
