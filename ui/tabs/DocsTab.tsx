import { useRef, useState } from 'react';
import {
  Badge,
  Button,
  Divider,
  DropdownMenu,
  Field,
  FileEntryIcon,
  FilePreview,
  FileTextIcon,
  FileThumb,
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
  UploadIcon,
  cn,
  useLiveQuery,
  useT,
  type FileEntry,
  type ServiceContextProps,
  type TextPayload,
  type ViewerKind,
} from '@holisdk/ui';
import type { DocsResponse, Document, UploadResponse } from '../types';

// The document pool — workflow stage 1. It takes in the room's knowledge three equal ways, exactly
// as the axioms require of any surface that holds a collection of files: a picker, drag-and-drop onto
// the list, and paste from the clipboard (a screenshot lands the same as a chosen PDF). Typed text
// stays a fourth way in, under the same single "Add" access point (no similar siblings). A file is
// shown for what it is — an image as an image, a PDF readable — through the SDK's file viewers (a
// service never renders media itself). Reads and writes go through the shared, authenticated api
// client; the backend pool is passive and every write is atomic.

// The per-file upload limit, mirroring backend/internal/api/files.go (maxFileBytes / maxUploadFiles).
// The server stays the authority; these let the entry point NAME what it accepts and let the UI turn
// an over-limit file away before a byte is sent — so an oversized file never becomes a stalled upload
// the server has to abort mid-stream. Keep the two in sync (like USE_RIGHT mirrors the backend right).
const MAX_FILE_MIB = 20;
const MAX_FILE_BYTES = MAX_FILE_MIB * 1024 * 1024;
// The file kinds the pool accepts (aigentic reads these). A picker hint only — drag/drop and paste
// bypass it and the server re-checks every file by sniffing its bytes.
const ACCEPT_HINT =
  'image/png,image/jpeg,image/gif,image/webp,application/pdf,text/*,.md,.markdown,.csv,.json,.log,.yaml,.yml,.toml,.ini';

// formatSize renders a byte count the way a user reads it — "70 MB", "1.4 MB" — for the rejection
// message, so a turned-away file shows its actual weight next to the limit.
function formatSize(bytes: number): string {
  const mb = bytes / (1024 * 1024);
  return `${mb >= 10 ? Math.round(mb) : mb.toFixed(1)} MB`;
}

// viewerFor maps a stored mime to the SDK viewer that renders it, so an uploaded file previews as
// what it is. Anything without a viewer still lists and downloads (the pool only ever stores kinds
// the assistant can read, so this is just presentation).
function viewerFor(mime: string): ViewerKind | null {
  if (mime.startsWith('image/')) return 'image';
  if (mime === 'application/pdf') return 'pdf';
  if (mime.startsWith('text/')) return mime === 'text/markdown' ? 'markdown' : 'text';
  return null;
}

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

  const [uploading, setUploading] = useState(false);
  const [dragging, setDragging] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // The full-screen viewer for a file document, and its lazily-loaded text (for text/markdown files,
  // whose bytes live out of band). null => closed.
  const [preview, setPreview] = useState<FileEntry | null>(null);
  const [previewText, setPreviewText] = useState<TextPayload | null>(null);

  // A file document, as the SDK file components expect it: the id is its virtual path (→ the raw
  // endpoint), the mime picks the viewer.
  function toEntry(d: Document): FileEntry {
    return { name: d.title, path: d.id, kind: 'file', size: d.size, mtime: d.created * 1000, mime: d.mime, viewer: viewerFor(d.mime) };
  }
  const rawUrl = (id: string) => api.url(`docs/${id}/raw`);
  const thumbSources = { mediaUrl: (e: FileEntry) => rawUrl(e.path) };

  function openPreview(d: Document) {
    const e = toEntry(d);
    setPreview(e);
    setPreviewText(null);
    if (e.viewer === 'text' || e.viewer === 'markdown') {
      api
        .raw(`docs/${d.id}/raw`)
        .then((r) => r.text())
        .then((text) => setPreviewText({ content: text }))
        .catch(() => setPreviewText({ content: '' }));
    }
  }

  // The one upload path shared by the picker, drag-and-drop and paste. An over-limit file is turned
  // away HERE, before any byte is sent — the browser already knows every File.size, so there is no
  // wait for something that would only be rejected (and no torn stream from the server aborting a
  // too-large body mid-upload). What is left within the limit is sent; unusable kinds are reported by
  // the backend with a reason; a fully-rejected batch throws (its detail is shown).
  async function upload(files: File[]) {
    if (files.length === 0 || uploading) return;
    const tooBig = files.filter((f) => f.size > MAX_FILE_BYTES);
    const within = files.filter((f) => f.size <= MAX_FILE_BYTES);
    if (tooBig.length > 0) {
      ui.toast({
        title: t('presentr.uploadTooBig'),
        description: tooBig
          .map((f) => t('presentr.uploadTooBigItem', { name: f.name, size: formatSize(f.size), limit: MAX_FILE_MIB }))
          .join('\n'),
        variant: 'error',
      });
    }
    if (within.length === 0) return;
    setUploading(true);
    try {
      const fd = new FormData();
      for (const f of within) fd.append('files', f, f.name);
      const res = await api.post<UploadResponse>('docs', fd);
      docsQ.refresh();
      const rejected = res.rejected ?? [];
      if (rejected.length > 0) {
        ui.toast({
          title: t('presentr.uploadPartial'),
          description: rejected.map((r) => `${r.name}: ${r.reason}`).join('\n'),
          variant: 'info',
        });
      }
    } catch (e) {
      ui.toast({ title: t('presentr.uploadFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setUploading(false);
    }
  }

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
  // selection, Home/End jump to the ends, Enter opens a file's viewer, Delete/Backspace removes it.
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
    } else if (e.key === 'Enter' && selected?.kind === 'file') {
      e.preventDefault();
      openPreview(selected);
    } else if ((e.key === 'Delete' || e.key === 'Backspace') && selectedId) {
      e.preventDefault();
      void remove(selectedId);
    }
  }

  // Drag-and-drop + paste onto the list (Drag & Drop für Dateisammlungen; Einfügen aus der
  // Zwischenablage): a dropped or pasted file lands the same as a picked one.
  function onDrop(e: React.DragEvent) {
    e.preventDefault();
    setDragging(false);
    const files = Array.from(e.dataTransfer.files);
    if (files.length) void upload(files);
  }
  function onPaste(e: React.ClipboardEvent) {
    const files = Array.from(e.clipboardData.files);
    if (files.length) {
      e.preventDefault();
      void upload(files);
    }
  }

  return (
    <Stack gap={4}>
      <Stack direction="row" justify="between" align="start" gap={3}>
        <Stack gap={0}>
          <Text weight="semibold">{t('presentr.docsHeading')}</Text>
          <Text variant="caption" color="tertiary">
            {t('presentr.docsSubtitle')}
          </Text>
        </Stack>
        <input
          ref={fileInputRef}
          type="file"
          multiple
          accept={ACCEPT_HINT}
          className="hidden"
          onChange={(e) => {
            const files = Array.from(e.target.files ?? []);
            e.target.value = '';
            if (files.length) void upload(files);
          }}
        />
        <Stack gap={1} align="end">
          <DropdownMenu
            align="end"
            trigger={
              <Button variant="primary" iconLeft={<PlusIcon />} loading={uploading}>
                {t('presentr.add')}
              </Button>
            }
            items={[
              { id: 'text', label: t('presentr.addText'), icon: <FileTextIcon />, onSelect: () => setAdding(true) },
              { id: 'files', label: t('presentr.uploadFiles'), icon: <UploadIcon />, onSelect: () => fileInputRef.current?.click() },
            ]}
          />
          {/* The access point names what it accepts — kinds and size — so the limit is known before it
              is reached (Intuitiv by Design), not discovered through a failed upload. */}
          <Text variant="caption" color="tertiary">
            {t('presentr.uploadHint', { limit: MAX_FILE_MIB })}
          </Text>
        </Stack>
      </Stack>

      <Stack direction="row" gap={4} align="stretch">
        <Panel
          className={cn('w-72 shrink-0 p-2 transition-colors', dragging && 'ring-2 ring-accent bg-accent/5')}
          onDragOver={(e) => {
            e.preventDefault();
            setDragging(true);
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={onDrop}
        >
          <Stack
            gap={1}
            role="listbox"
            tabIndex={0}
            aria-label={t('presentr.docsHeading')}
            onKeyDown={onListKeyDown}
            onPaste={onPaste}
            className="outline-none"
          >
            {docs.length > 0 ? (
              docs.map((d) => (
                <DocRow
                  key={d.id}
                  doc={d}
                  selected={d.id === selectedId}
                  onSelect={() => setSelectedId(d.id)}
                  onOpen={() => (d.kind === 'file' ? openPreview(d) : setSelectedId(d.id))}
                  onDelete={() => remove(d.id)}
                  deleteLabel={t('presentr.delete')}
                />
              ))
            ) : (
              <Text color="secondary" variant="footnote">
                {docsQ.loading ? '…' : dragging ? t('presentr.dropHint') : t('presentr.noDocs')}
              </Text>
            )}
          </Stack>
        </Panel>

        <Panel className="flex-1 min-w-0 p-4" title={selected?.title}>
          {selected ? (
            <Stack gap={3}>
              <Stack direction="row" align="center" gap={2} wrap>
                <Badge variant="neutral">{selected.kind === 'file' ? selected.mime : selected.kind}</Badge>
                <Text variant="caption" color="tertiary">
                  {t('presentr.byAuthor', { author: selected.author })} · {new Date(selected.created * 1000).toLocaleString()}
                </Text>
              </Stack>
              {selected.description && <Text color="secondary">{selected.description}</Text>}
              <Divider />
              {selected.kind === 'file' ? (
                <Stack gap={3} align="start">
                  <FileThumb entry={toEntry(selected)} sources={thumbSources} iconClassName="h-16 w-16" className="max-h-72 w-full" />
                  <Button variant="secondary" onClick={() => openPreview(selected)}>
                    {t('presentr.openFile')}
                  </Button>
                </Stack>
              ) : (
                <Markdown text={selected.content} />
              )}
            </Stack>
          ) : (
            <Text color="secondary">{t('presentr.selectDoc')}</Text>
          )}
        </Panel>
      </Stack>

      <FilePreview
        open={!!preview}
        entry={preview}
        rawUrl={preview ? rawUrl(preview.path) : undefined}
        text={previewText}
        onOpenChange={(o) => {
          if (!o) setPreview(null);
        }}
        onDownload={(e) => window.open(rawUrl(e.path), '_blank', 'noopener')}
      />

      <Modal
        open={adding}
        onOpenChange={setAdding}
        title={t('presentr.addText')}
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
  onOpen,
  onDelete,
  deleteLabel,
}: {
  doc: Document;
  selected: boolean;
  onSelect: () => void;
  onOpen: () => void;
  onDelete: () => void;
  deleteLabel: string;
}) {
  const entry: FileEntry =
    doc.kind === 'file'
      ? { name: doc.title, path: doc.id, kind: 'file', size: doc.size, mtime: doc.created * 1000, mime: doc.mime, viewer: viewerFor(doc.mime) }
      : { name: doc.title, path: doc.id, kind: 'file', size: doc.size, mtime: doc.created * 1000, mime: 'text/markdown', viewer: 'markdown' };
  return (
    <Stack
      role="option"
      aria-selected={selected}
      direction="row"
      align="center"
      justify="between"
      gap={2}
      onClick={onSelect}
      onDoubleClick={onOpen}
      className={cn('px-2 py-1.5 rounded-md cursor-pointer', selected ? 'bg-accent/15 text-text-primary' : 'hover:bg-fill/10')}
    >
      <Stack direction="row" align="center" gap={2} className="min-w-0">
        <FileEntryIcon entry={entry} className="h-4 w-4 shrink-0" />
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
