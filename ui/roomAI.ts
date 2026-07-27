// The room's AI access point. presentr asks the shared aigentic service two things — "explain the
// room" (Chat) and "derive the wiring from the documents" (Connection) — and both must ground the
// model in the SAME source: the room's document pool. This module is that single access point, so
// the grounding and the aigentic /run envelope live in ONE place instead of being re-derived per
// tab (Zugangspunkt wiederverwenden; the two tabs are no longer similar siblings).
//
// aigentic's /run contract is mirrored locally (see types.ts), never imported: a service UI may
// depend only on @holisdk/ui and react. Every answer carries the model that produced it
// (Kennzeichnungspflicht für KI-Modellantworten) — the caller labels its bubble/turn from
// AigenticResult.model/engine.
import type { ServiceContextProps } from '@holisdk/ui';
import type { AigenticRequest, AigenticResult, Document, PrizmEnvelope } from './types';

// The "Ask AI" standard: let aigentic pick the best engine it can offer (local ollama or Claude).
const ENGINE = 'choose';

// The room's documents as grounding context for the model: every document that carries text, as an
// inline markdown part. This is what makes an answer specific to THIS room rather than generic.
export function roomGrounding(docs: Document[]): NonNullable<AigenticRequest['inline']> {
  return docs
    .filter((d) => d.content && d.content.trim())
    .map((d) => ({ path: d.title || 'document', content: d.content, mediaType: 'text/markdown' }));
}

// Ask the room assistant. One place owns the engine kind, the prizm envelope and unwrapping its
// result, so Chat and Connection differ only in their prompt, grounding and requested output format.
export async function askRoom(apiFor: ServiceContextProps['apiFor'], req: AigenticRequest): Promise<AigenticResult> {
  const env = await apiFor('aigentic').post<PrizmEnvelope<AigenticResult>>('run', {
    header: { kind: ENGINE },
    data: req,
  });
  return env?.data ?? { output: '' };
}
