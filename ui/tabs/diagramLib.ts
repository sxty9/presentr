// Pure helpers for the connection diagram: geometry (so nodes and their port dots and the edge
// lines all agree on coordinates), a grid layout for freshly generated nodes, id minting, and the
// aigentic extraction prompt + tolerant parser. Kept out of the component so the maths is obvious
// and the component stays about interaction.
import type { DiagramGraph, DiagramNode, DiagramPort, Document } from '../types';

// Fixed device-box size; ports sit on the box's bottom edge and edges connect those points.
export const NODE_W = 168;
export const NODE_H = 84;

// A short random id for a device or port authored in the browser.
export function uid(): string {
  return Math.random().toString(36).slice(2, 10);
}

// n named ports for a new device.
export function makePorts(count: number): DiagramPort[] {
  const ports: DiagramPort[] = [];
  for (let i = 0; i < count; i++) ports.push({ id: uid(), name: `Port ${i + 1}` });
  return ports;
}

// A stable fingerprint of the document pool, so the diagram can tell when its baseline is stale.
export function sourceKeyOf(docs: Document[]): string {
  return docs
    .map((d) => `${d.id}:${d.created}`)
    .sort()
    .join('|');
}

// Grid layout for nodes that carry no position yet (the AI extraction returns none).
export function layout(nodes: DiagramNode[]): DiagramNode[] {
  const COLS = 4;
  const GAP_X = 220;
  const GAP_Y = 170;
  const X0 = 32;
  const Y0 = 32;
  return nodes.map((n, i) => {
    if (n.x || n.y) return n;
    const col = i % COLS;
    const row = Math.floor(i / COLS);
    return { ...n, x: X0 + col * GAP_X, y: Y0 + row * GAP_Y };
  });
}

// Absolute canvas coordinate of a device's port, distributed along the box's bottom edge.
export function portPoint(node: DiagramNode, index: number, count: number): { x: number; y: number } {
  const t = (index + 1) / (count + 1);
  return { x: node.x + NODE_W * t, y: node.y + NODE_H };
}

// The straight line between two points expressed as CSS box placement: a thin box anchored at `a`,
// as long as the distance, rotated to point at `b`. This is how the diagram draws edges without SVG.
export function lineBox(a: { x: number; y: number }, b: { x: number; y: number }) {
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const length = Math.sqrt(dx * dx + dy * dy);
  const angle = (Math.atan2(dy, dx) * 180) / Math.PI;
  return { left: a.x, top: a.y, width: length, angle, mid: { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 } };
}

// The extent the canvas must span to show the whole graph, with padding.
export function canvasSize(nodes: DiagramNode[]): { w: number; h: number } {
  let w = 640;
  let h = 360;
  for (const n of nodes) {
    w = Math.max(w, n.x + NODE_W + 48);
    h = Math.max(h, n.y + NODE_H + 64);
  }
  return { w, h };
}

// The aigentic prompt that turns the room documents into a device/connection graph. It points the
// model FIRST at the room's wiring evidence — a cabling diagram or a wiring photo, and any list that
// names the devices — because in a real room that evidence is a PICTURE (the attachments carry the
// pictures read out of the documents, so a vision engine sees them). It asks for a cable's type/colour
// as the edge label, and for a short `note` explaining the outcome — which becomes the persistent
// message the user reads when no connections could be concluded (Kein stummes Ausbleiben).
export const EXTRACT_PROMPT =
  'You are analysing the documents AND images that describe a presentation room, to reconstruct its ' +
  'connection diagram. Study every WIRING or CABLING drawing or photo you are given (a picture that ' +
  'shows devices joined by cables/lines) and any list that NAMES the devices — these are your primary ' +
  'evidence, and the pictures are attached to this request. Identify the physical DEVICES (for example: ' +
  'display or TV, projector, camera, microphone, speaker or soundbar, laptop, control panel or touch ' +
  'controller, hub, matrix, media player or mini-PC, wall plate, wireless-presentation receiver) and how ' +
  'they are CONNECTED to one another. Use each cable’s type or colour as the connection label when the ' +
  'documents show it (for example HDMI, USB, network/LAN, audio/XLR, power). Prefer the exact device ' +
  'NAMES the documents use.\n' +
  'Reply with ONLY a JSON object — no prose, no code fence — of exactly this shape:\n' +
  '{"nodes":[{"id":"n1","name":"Display","symbol":"TV","ports":[{"id":"n1p1","name":"HDMI 1"}]}],' +
  '"edges":[{"from":"n1","fromPort":"n1p1","to":"n2","toPort":"n2p1","label":"HDMI"}],' +
  '"note":"one short sentence stating what you concluded, or — if you found none — why"}\n' +
  'Rules: ids are short and unique; every edge references existing node and port ids; symbol is a ' +
  '1–2 character glyph; give each device a port for each real connector. If the documents genuinely do ' +
  'not describe the room’s wiring, return {"nodes":[],"edges":[],"note":"<why, e.g. no wiring diagram ' +
  'or device list was found in the documents>"}.';

// Parse the model's answer into a graph plus its note, tolerating prose or a code fence around the
// JSON. Returns null when nothing usable is found. The backend sanitises further (drops dangling
// edges, clamps). The note is the assistant's own account of the outcome — shown to the user verbatim
// when no connections were concluded, so the empty case is never a silent, endless spinner.
export function parseGraph(output: string): (DiagramGraph & { note?: string }) | null {
  if (!output) return null;
  const start = output.indexOf('{');
  const end = output.lastIndexOf('}');
  if (start < 0 || end <= start) return null;
  try {
    const obj = JSON.parse(output.slice(start, end + 1)) as Partial<DiagramGraph> & { note?: unknown };
    return {
      nodes: Array.isArray(obj.nodes) ? obj.nodes : [],
      edges: Array.isArray(obj.edges) ? obj.edges : [],
      note: typeof obj.note === 'string' ? obj.note : undefined,
    };
  } catch {
    return null;
  }
}
