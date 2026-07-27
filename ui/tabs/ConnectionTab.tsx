import { useEffect, useRef, useState, type PointerEvent } from 'react';
import {
  Badge,
  Box,
  Button,
  EmptyState,
  Field,
  IconButton,
  Input,
  Modal,
  NetworkIcon,
  Panel,
  PlusIcon,
  Spinner,
  Stack,
  Text,
  Tooltip,
  XIcon,
  cn,
  useT,
  type ServiceContextProps,
} from '@holisdk/ui';
import type {
  DiagramEdge,
  DiagramGraph,
  DiagramNode,
  DiagramView,
  DocsResponse,
} from '../types';
import { askRoom, roomGrounding } from '../roomAI';
import {
  EXTRACT_PROMPT,
  NODE_H,
  NODE_W,
  canvasSize,
  layout,
  lineBox,
  makePorts,
  parseGraph,
  portPoint,
  sourceKeyOf,
  uid,
} from './diagramLib';

// The connection diagram — workflow stage 2. An AI investigation of the document pool ("Ask AI"
// standard, via aigentic) proposes the devices and how they wire together; the user then edits the
// diagram by hand — drag devices, click two ports to connect them, add or remove devices. The first
// manual edit flips the diagram from the document-derived state to "manually modified", and the
// document state stays one click away (Restore).
//
// The SDK has no graph/canvas primitive and service UIs may not render raw SVG, so the diagram is
// drawn purely from SDK primitives: each device is an absolutely-positioned Box, and each connection
// is a thin Box rotated to point from one port to the other (see diagramLib.lineBox).

export function ConnectionTab({ api, apiFor, ui }: Pick<ServiceContextProps, 'api' | 'apiFor' | 'ui'>) {
  const t = useT();
  const [view, setView] = useState<DiagramView | null>(null); // null while loading
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState<{ node: string; port: string } | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [name, setName] = useState('');
  const [symbol, setSymbol] = useState('');
  const [portCount, setPortCount] = useState('2');
  const graphRef = useRef<DiagramGraph>({ nodes: [], edges: [] });
  const dragRef = useRef<null | { id: string; px: number; py: number; ox: number; oy: number; moved: boolean }>(null);

  useEffect(() => {
    let alive = true;
    api
      .get<DiagramView>('diagram')
      .then((v) => alive && setView(v))
      .catch(() => alive && setView(null));
    return () => {
      alive = false;
    };
  }, [api]);

  useEffect(() => {
    if (view) graphRef.current = { nodes: view.nodes, edges: view.edges };
  }, [view]);

  const nodes = view?.nodes ?? [];
  const edges = view?.edges ?? [];

  // Commit a manual edit: optimistic local update (=> manually-modified), then persist and adopt the
  // server's authoritative view.
  async function commit(next: DiagramGraph) {
    setView((v) => (v ? { ...v, nodes: next.nodes, edges: next.edges, state: 'manual', modified: true } : v));
    graphRef.current = next;
    setPending(null);
    try {
      const v = await api.put<DiagramView>('diagram', { nodes: next.nodes, edges: next.edges });
      setView(v);
    } catch (e) {
      ui.toast({ title: t('presentr.diagramSaveFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  function addDevice() {
    const nm = name.trim();
    if (!nm) return;
    const count = Math.max(0, Math.min(24, parseInt(portCount, 10) || 0));
    const idx = nodes.length;
    const node: DiagramNode = {
      id: uid(),
      name: nm,
      symbol: symbol.trim().slice(0, 2) || nm.slice(0, 1).toUpperCase(),
      x: 32 + (idx % 4) * 220,
      y: 32 + Math.floor(idx / 4) * 170,
      ports: makePorts(count),
    };
    setAddOpen(false);
    setName('');
    setSymbol('');
    setPortCount('2');
    void commit({ nodes: [...nodes, node], edges });
  }

  function deleteNode(id: string) {
    void commit({ nodes: nodes.filter((n) => n.id !== id), edges: edges.filter((e) => e.from !== id && e.to !== id) });
  }

  function deleteEdge(id: string) {
    void commit({ nodes, edges: edges.filter((e) => e.id !== id) });
  }

  // Click a port: the first click arms it, the second (on a different device) draws the cable.
  function clickPort(nodeId: string, portId: string) {
    if (!pending || pending.node === nodeId) {
      setPending({ node: nodeId, port: portId });
      return;
    }
    const dup = edges.some(
      (e) =>
        (e.from === pending.node && e.fromPort === pending.port && e.to === nodeId && e.toPort === portId) ||
        (e.to === pending.node && e.toPort === pending.port && e.from === nodeId && e.fromPort === portId),
    );
    if (dup) {
      setPending(null);
      return;
    }
    const edge: DiagramEdge = { id: uid(), from: pending.node, fromPort: pending.port, to: nodeId, toPort: portId };
    void commit({ nodes, edges: [...edges, edge] });
  }

  // Drag a device. Positions update locally on every move (no round-trip); the new layout is
  // persisted once, on release.
  function onNodeDown(e: PointerEvent<HTMLElement>, node: DiagramNode) {
    e.stopPropagation();
    dragRef.current = { id: node.id, px: e.clientX, py: e.clientY, ox: node.x, oy: node.y, moved: false };
    e.currentTarget.setPointerCapture?.(e.pointerId);
  }
  function onNodeMove(e: PointerEvent<HTMLElement>) {
    const d = dragRef.current;
    if (!d) return;
    if (Math.abs(e.clientX - d.px) > 3 || Math.abs(e.clientY - d.py) > 3) d.moved = true;
    const nx = Math.max(0, d.ox + (e.clientX - d.px));
    const ny = Math.max(0, d.oy + (e.clientY - d.py));
    setView((v) => (v ? { ...v, nodes: v.nodes.map((n) => (n.id === d.id ? { ...n, x: nx, y: ny } : n)) } : v));
  }
  function onNodeUp(e: PointerEvent<HTMLElement>) {
    const d = dragRef.current;
    if (!d) return;
    dragRef.current = null;
    e.currentTarget.releasePointerCapture?.(e.pointerId);
    if (d.moved) void commit(graphRef.current);
  }

  // Derive the diagram from the documents via aigentic, then install it as the new baseline.
  async function generate() {
    if (busy) return;
    setBusy(true);
    try {
      const docs = (await api.get<DocsResponse>('docs')).docs ?? [];
      if (docs.length === 0) {
        ui.toast({ title: t('presentr.diagramNeedDocs'), variant: 'info' });
        return;
      }
      const result = await askRoom(apiFor, { prompt: EXTRACT_PROMPT, inline: roomGrounding(docs), outputFormat: 'json' });
      const g = parseGraph(result.output ?? '');
      if (!g || g.nodes.length === 0) {
        ui.toast({ title: t('presentr.diagramNoResult'), variant: 'info' });
        return;
      }
      const v = await api.post<DiagramView>('diagram/generate', {
        nodes: layout(g.nodes),
        edges: g.edges,
        sourceKey: sourceKeyOf(docs),
      });
      setView(v);
    } catch (e) {
      ui.toast({ title: t('presentr.diagramGenFailed'), description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function restore() {
    try {
      setView(await api.post<DiagramView>('diagram/restore', {}));
    } catch (e) {
      ui.toast({ title: t('presentr.diagramRestoreFailed'), description: (e as Error).message, variant: 'error' });
    }
  }

  if (view === null) {
    return (
      <Panel className="p-8">
        <Stack align="center">
          <Spinner />
        </Stack>
      </Panel>
    );
  }

  const size = canvasSize(nodes);
  const points = new Map<string, { x: number; y: number }>();
  for (const n of nodes) n.ports.forEach((p, i) => points.set(`${n.id}:${p.id}`, portPoint(n, i, n.ports.length)));

  return (
    <Stack gap={3}>
      <Stack direction="row" justify="between" align="center" gap={3} wrap>
        <Stack direction="row" align="center" gap={2}>
          <Text weight="semibold">{t('presentr.diagramHeading')}</Text>
          <Badge variant={view.modified ? 'warning' : 'success'}>
            {view.modified ? t('presentr.stateManual') : t('presentr.stateDocument')}
          </Badge>
        </Stack>
        <Stack direction="row" align="center" gap={2} wrap>
          {view.modified && (
            <Button variant="ghost" onClick={restore}>
              {t('presentr.restore')}
            </Button>
          )}
          <Button variant="secondary" iconLeft={<PlusIcon />} onClick={() => setAddOpen(true)}>
            {t('presentr.addDevice')}
          </Button>
          <Button variant="primary" loading={busy} onClick={generate}>
            {t('presentr.generate')}
          </Button>
        </Stack>
      </Stack>

      <Panel className="p-0 overflow-auto" style={{ maxHeight: '60vh' }}>
        <Box className="relative select-none" style={{ width: size.w, height: size.h }} onPointerDown={() => setPending(null)}>
          {nodes.length === 0 && (
            <Box className="absolute inset-0">
              <Stack align="center" justify="center" className="h-full p-8">
                <EmptyState icon={<NetworkIcon />} title={t('presentr.diagramEmptyTitle')} description={t('presentr.diagramEmptyBody')} />
              </Stack>
            </Box>
          )}

          {/* connections (behind the devices) */}
          {edges.map((e) => {
            const a = points.get(`${e.from}:${e.fromPort}`);
            const b = points.get(`${e.to}:${e.toPort}`);
            if (!a || !b) return null;
            const g = lineBox(a, b);
            return (
              <Box
                key={e.id}
                className="absolute bg-fill/40"
                style={{ left: g.left, top: g.top, width: g.width, height: 2, transform: `rotate(${g.angle}deg)`, transformOrigin: '0 0' }}
              />
            );
          })}

          {/* devices */}
          {nodes.map((n) => (
            <Box
              key={n.id}
              className="absolute rounded-lg border border-separator bg-surface-raised shadow-elev-1"
              style={{ left: n.x, top: n.y, width: NODE_W, height: NODE_H, touchAction: 'none', cursor: 'grab' }}
              onPointerDown={(e) => onNodeDown(e, n)}
              onPointerMove={onNodeMove}
              onPointerUp={onNodeUp}
            >
              <Stack className="h-full p-2" gap={1} justify="center">
                <Stack direction="row" align="center" gap={2}>
                  <Badge variant="accent">{n.symbol || '?'}</Badge>
                  <Text weight="semibold" truncate>
                    {n.name}
                  </Text>
                </Stack>
                <Text variant="caption" color="tertiary">
                  {n.ports.length} {t('presentr.ports')}
                </Text>
              </Stack>
              <Box className="absolute" style={{ right: -10, top: -10 }}>
                <IconButton
                  label={t('presentr.deleteDevice')}
                  size="sm"
                  variant="ghost"
                  onPointerDown={(e) => e.stopPropagation()}
                  onClick={(e) => {
                    e.stopPropagation();
                    deleteNode(n.id);
                  }}
                >
                  <XIcon />
                </IconButton>
              </Box>
            </Box>
          ))}

          {/* connection delete handles (above the devices) */}
          {edges.map((e) => {
            const a = points.get(`${e.from}:${e.fromPort}`);
            const b = points.get(`${e.to}:${e.toPort}`);
            if (!a || !b) return null;
            const g = lineBox(a, b);
            return (
              <Box key={`del-${e.id}`} className="absolute" style={{ left: g.mid.x - 12, top: g.mid.y - 12 }}>
                <IconButton label={t('presentr.deleteConnection')} size="sm" variant="ghost" onClick={() => deleteEdge(e.id)}>
                  <XIcon />
                </IconButton>
              </Box>
            );
          })}

          {/* ports (topmost, clickable) */}
          {nodes.map((n) =>
            n.ports.map((p, i) => {
              const pt = portPoint(n, i, n.ports.length);
              const armed = pending?.node === n.id && pending?.port === p.id;
              return (
                <Box key={`${n.id}:${p.id}`} className="absolute" style={{ left: pt.x - 7, top: pt.y - 7 }}>
                  <Tooltip content={p.name}>
                    <Box
                      role="button"
                      aria-label={p.name}
                      className={cn('rounded-full border cursor-pointer', armed ? 'bg-accent border-accent ring-2 ring-accent' : 'bg-surface border-separator hover:bg-accent')}
                      style={{ width: 14, height: 14 }}
                      onPointerDown={(e) => e.stopPropagation()}
                      onClick={(e) => {
                        e.stopPropagation();
                        clickPort(n.id, p.id);
                      }}
                    />
                  </Tooltip>
                </Box>
              );
            }),
          )}
        </Box>
      </Panel>

      {pending && (
        <Text variant="footnote" color="secondary">
          {t('presentr.connectHint')}
        </Text>
      )}

      <Modal
        open={addOpen}
        onOpenChange={setAddOpen}
        title={t('presentr.addDevice')}
        footer={
          <Stack direction="row" justify="end" gap={2}>
            <Button variant="ghost" onClick={() => setAddOpen(false)}>
              {t('presentr.cancel')}
            </Button>
            <Button variant="primary" disabled={!name.trim()} onClick={addDevice}>
              {t('presentr.add')}
            </Button>
          </Stack>
        }
      >
        <Stack gap={3}>
          <Field label={t('presentr.deviceName')}>
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={t('presentr.deviceNamePlaceholder')} />
          </Field>
          <Field label={t('presentr.deviceSymbol')}>
            <Input value={symbol} onChange={(e) => setSymbol(e.target.value)} placeholder="P" />
          </Field>
          <Field label={t('presentr.devicePorts')}>
            <Input type="number" value={portCount} onChange={(e) => setPortCount(e.target.value)} />
          </Field>
        </Stack>
      </Modal>
    </Stack>
  );
}
