import { useState } from 'react';
import {
  Badge,
  Button,
  Divider,
  Field,
  IconButton,
  Input,
  Markdown,
  Modal,
  Panel,
  PlusIcon,
  Stack,
  Text,
  Textarea,
  TrashIcon,
  cn,
  useLiveQuery,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import type { DocsResponse, Document } from '../types';

// The document pool — workflow stage 1. A keyboard-navigable list of the room's documents on the
// left, the selected document rendered as Markdown on the right, and an "Add text" dialog to grow
// the pool. Reads and writes go through the shared, already-authenticated api client; the backend
// pool is passive and every write is atomic.
export function DocsTab({ api, ui }: Pick<ServiceContextProps, 'api' | 'ui'>) {
  const t = useT();
  const docsQ = useLiveQuery<DocsResponse>(() => api.get<DocsResponse>('docs'), 8000);
  const docs = docsQ.data?.docs ?? [];

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selected = docs.find((d) => d.id === selectedId) ?? null;

  const [adding, setAdding] = useState(false);
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [content, setContent] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit() {
    const ti = title.trim();
    const co = content.trim();
    if (!ti || !co || busy) return;
    setBusy(true);
    try {
      await api.post('docs', { title: ti, content: co, description: description.trim() });
      setAdding(false);
      setTitle('');
      setDescription('');
      setContent('');
      docsQ.refresh();
    } catch (e) {
      ui.toast({ title: t('presentr.saveFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function remove(id: string) {
    const ok = await ui.confirm({
      title: t('presentr.deleteTitle'),
      description: t('presentr.deleteConfirm'),
      danger: true,
      confirmLabel: t('presentr.delete'),
    });
    if (!ok) return;
    try {
      await api.del(`docs/${id}`);
      if (selectedId === id) setSelectedId(null);
      docsQ.refresh();
    } catch (e) {
      ui.toast({ title: t('presentr.deleteFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  // Keyboard navigation for the list (Tastaturnavigation in Listenelementen): arrows move the
  // selection, Home/End jump to the ends, Delete/Backspace removes the selected document.
  function onListKeyDown(e: React.KeyboardEvent) {
    if (docs.length === 0) return;
    const idx = docs.findIndex((d) => d.id === selectedId);
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setSelectedId(docs[Math.min(idx + 1, docs.length - 1)]?.id ?? docs[0].id);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setSelectedId(idx <= 0 ? docs[0].id : docs[idx - 1].id);
    } else if (e.key === 'Home') {
      e.preventDefault();
      setSelectedId(docs[0].id);
    } else if (e.key === 'End') {
      e.preventDefault();
      setSelectedId(docs[docs.length - 1].id);
    } else if ((e.key === 'Delete' || e.key === 'Backspace') && selectedId) {
      e.preventDefault();
      void remove(selectedId);
    }
  }

  return (
    <Stack gap={4}>
      <Stack direction="row" justify="between" align="center" gap={3}>
        <Stack gap={0}>
          <Text weight="semibold">{t('presentr.docsHeading')}</Text>
          <Text variant="caption" color="tertiary">
            {t('presentr.docsSubtitle')}
          </Text>
        </Stack>
        <Button variant="primary" iconLeft={<PlusIcon />} onClick={() => setAdding(true)}>
          {t('presentr.addDoc')}
        </Button>
      </Stack>

      <Stack direction="row" gap={4} align="stretch">
        <Panel className="w-72 shrink-0 p-2">
          <Stack gap={1} role="listbox" tabIndex={0} aria-label={t('presentr.docsHeading')} onKeyDown={onListKeyDown}>
            {docs.length > 0 ? (
              docs.map((d) => (
                <DocRow key={d.id} doc={d} selected={d.id === selectedId} onSelect={() => setSelectedId(d.id)} onDelete={() => remove(d.id)} deleteLabel={t('presentr.delete')} />
              ))
            ) : (
              <Text color="secondary" variant="footnote">
                {docsQ.loading ? '…' : t('presentr.noDocs')}
              </Text>
            )}
          </Stack>
        </Panel>

        <Panel className="flex-1 min-w-0 p-4" title={selected?.title}>
          {selected ? (
            <Stack gap={3}>
              <Stack direction="row" align="center" gap={2} wrap>
                <Badge variant="neutral">{selected.kind}</Badge>
                <Text variant="caption" color="tertiary">
                  {t('presentr.byAuthor', { author: selected.author })} · {new Date(selected.created * 1000).toLocaleString()}
                </Text>
              </Stack>
              {selected.description && <Text color="secondary">{selected.description}</Text>}
              <Divider />
              <Markdown text={selected.content} />
            </Stack>
          ) : (
            <Text color="secondary">{t('presentr.selectDoc')}</Text>
          )}
        </Panel>
      </Stack>

      <Modal
        open={adding}
        onOpenChange={setAdding}
        title={t('presentr.addDoc')}
        footer={
          <Stack direction="row" justify="end" gap={2}>
            <Button variant="ghost" onClick={() => setAdding(false)}>
              {t('presentr.cancel')}
            </Button>
            <Button variant="primary" loading={busy} disabled={!title.trim() || !content.trim()} onClick={submit}>
              {t('presentr.save')}
            </Button>
          </Stack>
        }
      >
        <Stack gap={3}>
          <Field label={t('presentr.docTitle')}>
            <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder={t('presentr.docTitlePlaceholder')} />
          </Field>
          <Field label={t('presentr.docDescription')}>
            <Input value={description} onChange={(e) => setDescription(e.target.value)} placeholder={t('presentr.docDescriptionPlaceholder')} />
          </Field>
          <Field label={t('presentr.docContent')}>
            <Textarea rows={10} value={content} onChange={(e) => setContent(e.target.value)} placeholder={t('presentr.docContentPlaceholder')} />
          </Field>
        </Stack>
      </Modal>
    </Stack>
  );
}

function DocRow({
  doc,
  selected,
  onSelect,
  onDelete,
  deleteLabel,
}: {
  doc: Document;
  selected: boolean;
  onSelect: () => void;
  onDelete: () => void;
  deleteLabel: string;
}) {
  return (
    <Stack
      role="option"
      aria-selected={selected}
      direction="row"
      align="center"
      justify="between"
      gap={2}
      onClick={onSelect}
      className={cn('px-2 py-1.5 rounded-md cursor-pointer', selected ? 'bg-accent/15 text-text-primary' : 'hover:bg-fill/10')}
    >
      <Stack gap={0} className="min-w-0">
        <Text truncate weight={selected ? 'semibold' : 'normal'}>
          {doc.title}
        </Text>
        {doc.description && (
          <Text variant="caption" color="tertiary" truncate>
            {doc.description}
          </Text>
        )}
      </Stack>
      <IconButton
        label={deleteLabel}
        size="sm"
        variant="ghost"
        onClick={(e) => {
          e.stopPropagation();
          onDelete();
        }}
      >
        <TrashIcon />
      </IconButton>
    </Stack>
  );
}
