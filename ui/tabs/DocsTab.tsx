import { useEffect, useRef, useState } from 'react';
import {
  Badge,
  Box,
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
  ProgressBar,
  Stack,
  Text,
  Textarea,
  TrashIcon,
  UploadIcon,
  XIcon,
  cn,
  useLiveQuery,
  useT,
  type FileEntry,
  type ServiceApiClient,
  type ServiceContextProps,
  type TextPayload,
  type ViewerKind,
} from '@holistic/ui';
import type { DocsResponse, Document, ExtractResponse, UploadResponse } from '../types';

// The document pool — workflow stage 1. It takes in the room's knowledge three equal ways, exactly
// as the axioms require of any surface that holds a collection of files: a picker, drag-and-drop onto
// the list, and paste from the clipboard (a screenshot lands the same as a chosen PDF). Typed text
// stays a fourth way in, under the same single "Add" access point (no similar siblings). A file is
// shown for what it is — an image as an image, a PDF readable — through the SDK's file viewers (a
// service never renders media itself). Reads and writes go through the shared, authenticated api
// client; the backend pool is passive and every write is atomic.

// The per-file upload limit, mirroring backend/internal/api/files.go (maxFileBytes). The server stays
// the authority; this lets the entry point NAME what it accepts and lets the UI turn an over-limit
// file away before a byte is sent — so an oversized file never becomes a stalled upload the server
// has to abort mid-stream. Keep it in sync (like USE_RIGHT mirrors the backend right).
const MAX_FILE_MIB = 100;
const MAX_FILE_BYTES = MAX_FILE_MIB * 1024 * 1024;

// Send-progress transport. Real per-file progress (5% vs 95% of an 80 MB file) needs the browser to
// report how many bytes have gone out, which fetch cannot do — so the shared api client exposes an
// `upload` method (XMLHttpRequest under the hood) that reports progress and honours an AbortSignal.
// It is a shared-SDK capability (every service with uploads needs it), consumed here, not rebuilt.
// It is feature-detected: on a host that predates it, uploads still run (and cancel) through `post`,
// only without the live percentage — so the Docs tab degrades gracefully instead of breaking.
type UploadProgressOpts = { onProgress?: (loaded: number, total: number) => void; signal?: AbortSignal };
type UploadCapableApi = ServiceApiClient & {
  upload?<T>(path: string, body: FormData, opts?: UploadProgressOpts): Promise<T>;
};

// One file's journey through an upload, shown as its own row (name, size, progress, outcome). The
// states are visually distinct without any explaining text: queued and uploading advance a bar,
// added is a full success bar, rejected/canceled/failed stop with a coloured bar and a badge.
type UploadStatus = 'queued' | 'uploading' | 'done' | 'rejected' | 'canceled' | 'error';
interface UploadItem {
  key: string;
  name: string;
  size: number;
  loaded: number;
  status: UploadStatus;
  reason?: string;
  controller?: AbortController;
}
// The file kinds the pool accepts (aigentic reads these). A picker hint only — drag/drop and paste
// bypass it and the server re-checks every file by sniffing its bytes.
const ACCEPT_HINT =
  'image/png,image/jpeg,image/gif,image/webp,application/pdf,text/*,.md,.markdown,.csv,.json,.log,.yaml,.yml,.toml,.ini';

// Open the OS file dialog as an ACTION, not a rendered surface. A service UI composes only
// @holistic/ui components and renders no raw element — not even a hidden <input> — so the picker is
// built on demand inside the click handler, opened, and its chosen files read back. The opening is a
// Handlung, not a Fläche. (The SDK's UploadControl renders its OWN button + a folder option, which
// would duplicate this tab's single "Add" access point and offer a folder upload presentr does not
// take — so it does not fit here; Keine ähnlichen Geschwister.)
function openFileDialog(accept: string, onFiles: (files: File[]) => void) {
  const input = document.createElement('input');
  input.type = 'file';
  input.multiple = true;
  input.accept = accept;
  input.onchange = () => {
    const files = Array.from(input.files ?? []);
    if (files.length) onFiles(files);
  };
  input.click();
}

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

  const [dragging, setDragging] = useState(false);

  // The live per-file upload rows and a ref mirror of them, so the sequential queue runner and the
  // cancel handler can read the current status without going stale between renders.
  const [uploads, setUploads] = useState<UploadItem[]>([]);
  const uploadsRef = useRef<UploadItem[]>([]);
  useEffect(() => {
    uploadsRef.current = uploads;
  }, [uploads]);
  const uploadKey = useRef(0);
  const queueRef = useRef<Promise<void>>(Promise.resolve());
  const uploadApi = api as UploadCapableApi;
  const uploading = uploads.some((u) => u.status === 'queued' || u.status === 'uploading');

  function patchUpload(key: string, p: Partial<UploadItem>) {
    setUploads((list) => list.map((it) => (it.key === key ? { ...it, ...p } : it)));
  }
  function dismissUpload(key: string) {
    setUploads((list) => list.filter((it) => it.key !== key));
  }
  // Cancel a file mid-flight: an in-progress request is aborted (the browser stops sending); a file
  // still waiting in the queue is simply marked canceled, and the runner skips it when its turn comes.
  function cancelUpload(key: string) {
    const it = uploadsRef.current.find((u) => u.key === key);
    if (it?.controller) it.controller.abort();
    else patchUpload(key, { status: 'canceled' });
  }

  // Send exactly one file as its own request, so each file has its own progress and can be canceled
  // independently. Uses the SDK's progress-reporting upload when the host provides it, else falls back
  // to a plain post (still cancelable via the AbortSignal, just without the live percentage).
  async function sendOne(key: string, file: File) {
    if (uploadsRef.current.find((u) => u.key === key)?.status === 'canceled') return;
    const controller = new AbortController();
    patchUpload(key, { status: 'uploading', loaded: 0, controller });
    const fd = new FormData();
    fd.append('files', file, file.name);
    try {
      if (uploadApi.upload) {
        await uploadApi.upload<UploadResponse>('docs', fd, {
          signal: controller.signal,
          // Track bytes sent against the file's own size; the request carries a little multipart
          // overhead on top, so the bar is clamped to 100% (UploadRow) rather than overshooting.
          onProgress: (loaded) => patchUpload(key, { loaded }),
        });
      } else {
        await api.post<UploadResponse>('docs', fd, { signal: controller.signal });
      }
      // A single accepted file answers 200; a rejected one answers an error (caught below). Mark the
      // bar complete and pull the new document into the list.
      patchUpload(key, { status: 'done', loaded: file.size, controller: undefined });
      docsQ.refresh();
    } catch (e) {
      const err = e as Error;
      if (controller.signal.aborted || err.name === 'AbortError') {
        patchUpload(key, { status: 'canceled', controller: undefined });
      } else {
        // The server names WHY (wrong kind, over the limit) in the error detail — surface it verbatim.
        patchUpload(key, { status: 'rejected', reason: err.message, controller: undefined });
      }
    }
  }

  // Take a batch of chosen/dropped/pasted files onto the per-file upload list. An over-limit file is
  // turned away HERE as its own rejected row (before any byte is sent), so it is visible alongside the
  // real uploads instead of a fire-and-forget toast; the rest are queued and sent one at a time.
  function enqueue(files: File[]) {
    if (files.length === 0) return;
    const rows: UploadItem[] = [];
    const toSend: { key: string; file: File }[] = [];
    for (const f of files) {
      const key = `u${uploadKey.current++}`;
      if (f.size > MAX_FILE_BYTES) {
        rows.push({
          key,
          name: f.name,
          size: f.size,
          loaded: 0,
          status: 'rejected',
          reason: t('presentr.uploadTooBigItem', { name: f.name, size: formatSize(f.size), limit: MAX_FILE_MIB }),
        });
      } else {
        rows.push({ key, name: f.name, size: f.size, loaded: 0, status: 'queued' });
        toSend.push({ key, file: f });
      }
    }
    setUploads((list) => [...rows, ...list]);
    // Chain onto the running queue so files always upload one after another, in order, even across
    // several drops — one clear "which of five is going now", never five competing bars at once.
    queueRef.current = queueRef.current.then(async () => {
      for (const { key, file } of toSend) await sendOne(key, file);
    });
  }

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
    if (files.length) enqueue(files);
  }
  function onPaste(e: React.ClipboardEvent) {
    const files = Array.from(e.clipboardData.files);
    if (files.length) {
      e.preventDefault();
      enqueue(files);
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
              { id: 'files', label: t('presentr.uploadFiles'), icon: <UploadIcon />, onSelect: () => openFileDialog(ACCEPT_HINT, enqueue) },
            ]}
          />
          {/* The access point names what it accepts — kinds and size — so the limit is known before it
              is reached (Intuitiv by Design), not discovered through a failed upload. */}
          <Text variant="caption" color="tertiary">
            {t('presentr.uploadHint', { limit: MAX_FILE_MIB })}
          </Text>
        </Stack>
      </Stack>

      {uploads.length > 0 && (
        <Panel className="p-3">
          <Stack gap={2}>
            <Stack direction="row" align="center" justify="between">
              <Text variant="caption" color="tertiary">
                {t('presentr.uploadsHeading')}
              </Text>
              {uploads.some((u) => u.status !== 'queued' && u.status !== 'uploading') && (
                <Button variant="ghost" size="sm" onClick={() => setUploads((list) => list.filter((u) => u.status === 'queued' || u.status === 'uploading'))}>
                  {t('presentr.uploadClearDone')}
                </Button>
              )}
            </Stack>
            {uploads.map((u) => (
              <UploadRow key={u.key} item={u} onCancel={() => cancelUpload(u.key)} onDismiss={() => dismissUpload(u.key)} t={t} />
            ))}
          </Stack>
        </Panel>
      )}

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
                  readingLabel={t('presentr.extractReading')}
                  unreadLabel={t('presentr.extractUnread')}
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
                  <Divider />
                  <ExtractSection api={api} ui={ui} doc={selected} t={t} onChanged={() => docsQ.refresh()} />
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
  readingLabel,
  unreadLabel,
}: {
  doc: Document;
  selected: boolean;
  onSelect: () => void;
  onOpen: () => void;
  onDelete: () => void;
  deleteLabel: string;
  readingLabel: string;
  unreadLabel: string;
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
      {/* At-a-glance read state for a file: a file being read, or one whose read failed, shows a badge;
          a successfully read file needs no attention and shows none (Intuitiv by Design). */}
      {doc.kind === 'file' && doc.extractState === 'pending' && (
        <Badge variant="neutral" className="shrink-0">
          {readingLabel}
        </Badge>
      )}
      {doc.kind === 'file' && doc.extractState === 'failed' && (
        <Badge variant="danger" className="shrink-0">
          {unreadLabel}
        </Badge>
      )}
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

// The extraction section of a file's detail panel: it shows the ONE named read state and lets the user
// act on it. Reading shows a quiet "reading…" note (the state ENDS — the list polls and this resolves);
// a failed read shows why and offers "Read again" (retry from the stored bytes, no re-upload); a
// finished read names its source (exact text layer, or recognized by a model) and can reveal the text
// the assistant will actually draw on. A file with no read yet (uploaded before this feature) offers to
// read it now.
function ExtractSection({
  api,
  ui,
  doc,
  t,
  onChanged,
}: {
  api: ServiceApiClient;
  ui: ServiceContextProps['ui'];
  doc: Document;
  t: ReturnType<typeof useT>;
  onChanged: () => void;
}) {
  const [showing, setShowing] = useState(false);
  const [text, setText] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [retrying, setRetrying] = useState(false);
  const state = doc.extractState ?? '';

  // Reset the revealed text when the selected document changes, so it never shows a stale extract.
  useEffect(() => {
    setShowing(false);
    setText(null);
  }, [doc.id]);

  async function toggle() {
    if (showing) {
      setShowing(false);
      return;
    }
    setShowing(true);
    if (text === null) {
      setLoading(true);
      try {
        const r = await api.get<ExtractResponse>(`docs/${doc.id}/extract`);
        setText(r.text ?? '');
      } catch {
        setText('');
        ui.toast({ title: t('presentr.extractLoadFailed'), variant: 'error' });
      } finally {
        setLoading(false);
      }
    }
  }

  async function retry() {
    setRetrying(true);
    try {
      await api.post(`docs/${doc.id}/extract`, {});
      setText(null);
      setShowing(false);
      onChanged();
    } catch (e) {
      ui.toast({ title: t('presentr.extractRetryFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setRetrying(false);
    }
  }

  if (state === 'pending') {
    return (
      <Stack gap={1} align="start">
        <Badge variant="neutral">{t('presentr.extractReading')}</Badge>
        <Text variant="caption" color="tertiary">
          {t('presentr.extractPendingBody')}
        </Text>
      </Stack>
    );
  }

  if (state === 'failed') {
    return (
      <Stack gap={2} align="start">
        <Text color="secondary">{t('presentr.extractFailedBody', { reason: doc.extractError ?? '' })}</Text>
        <Button variant="secondary" loading={retrying} onClick={retry}>
          {t('presentr.extractRetry')}
        </Button>
      </Stack>
    );
  }

  if (state === 'ready') {
    const source =
      doc.extractSource === 'ai'
        ? t('presentr.extractFromAI', { model: doc.extractModel || doc.extractEngine || 'the assistant' })
        : t('presentr.extractFromLayer');
    return (
      <Stack gap={2} align="start" className="w-full">
        <Text weight="semibold">{t('presentr.extractReadHeading')}</Text>
        <Text variant="caption" color="tertiary">
          {source}
        </Text>
        <Button variant="ghost" size="sm" onClick={toggle} loading={loading}>
          {showing ? t('presentr.extractHide') : t('presentr.extractShow')}
        </Button>
        {showing && !loading && (
          <Box className="max-h-72 w-full overflow-auto rounded-md bg-fill/5 p-3">
            <Text variant="footnote" color="secondary" className="whitespace-pre-wrap">
              {text ? text : t('presentr.extractEmpty')}
            </Text>
          </Box>
        )}
      </Stack>
    );
  }

  // No read yet (a file uploaded before this feature): offer to read it now.
  return (
    <Button variant="secondary" loading={retrying} onClick={retry}>
      {t('presentr.extractRetry')}
    </Button>
  );
}

// One upload's row: name, size, a per-file progress bar and its outcome. The bar colour and the
// badge carry the state on their own — a full green bar means added, a stopped red bar means
// rejected, amber means canceled — so "done, running, rejected" are told apart at a glance without
// any explaining text (Intuitiv by Design). While a file is in flight the row offers Cancel; once it
// has settled it offers Dismiss.
function UploadRow({
  item,
  onCancel,
  onDismiss,
  t,
}: {
  item: UploadItem;
  onCancel: () => void;
  onDismiss: () => void;
  t: ReturnType<typeof useT>;
}) {
  const active = item.status === 'queued' || item.status === 'uploading';
  const failed = item.status === 'rejected' || item.status === 'error';
  const pct = item.size > 0 ? Math.min(100, Math.round((item.loaded / item.size) * 100)) : 0;
  // A bar with no byte count yet (queued, or an in-flight upload the host cannot report progress for)
  // pulses; otherwise it fills to the real percentage, or all the way for any settled state.
  const indeterminate = item.status === 'uploading' && item.loaded === 0;
  const tone = item.status === 'done' ? 'success' : failed ? 'danger' : item.status === 'canceled' ? 'warning' : 'accent';
  const badge = item.status === 'done' ? 'success' : failed ? 'danger' : item.status === 'canceled' ? 'warning' : 'neutral';
  const barValue = active ? pct : 100;
  return (
    <Stack gap={1}>
      <Stack direction="row" align="center" justify="between" gap={2}>
        <Stack direction="row" align="center" gap={2} className="min-w-0">
          <Text truncate>{item.name}</Text>
          <Text variant="caption" color="tertiary" className="shrink-0">
            {formatSize(item.size)}
          </Text>
        </Stack>
        <Stack direction="row" align="center" gap={2} className="shrink-0">
          {item.status === 'uploading' && !indeterminate && (
            <Text variant="caption" color="tertiary">
              {pct}%
            </Text>
          )}
          <Badge variant={badge}>{t(`presentr.uploadState_${item.status}`)}</Badge>
          <IconButton label={active ? t('presentr.uploadCancel') : t('presentr.uploadDismiss')} size="sm" variant="ghost" onClick={active ? onCancel : onDismiss}>
            <XIcon />
          </IconButton>
        </Stack>
      </Stack>
      <ProgressBar value={barValue} indeterminate={indeterminate} tone={tone} />
      {item.reason && (
        <Text variant="caption" color="tertiary">
          {item.reason}
        </Text>
      )}
    </Stack>
  );
}
