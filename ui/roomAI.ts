// The room's AI access point. presentr asks the room's assistant two things — "explain the room"
// (Chat) and "derive the wiring from the documents" (Connection) — and both must ground the model in
// the SAME source: the room's document pool. That grounding now lives on presentr's BACKEND (POST
// ask), because the pool holds uploaded PDFs and images whose bytes aigentic reads natively:
// assembling the grounding server-side keeps those bytes off the wire to the browser and back
// (Portionierte Daten) and keeps ONE access point to the room AI. So this module is a thin client of
// that endpoint — the Chat tab and the Connection diagram differ only in their prompt and requested
// output shape. Every answer carries the model that produced it; the caller labels its bubble/turn
// from AskResult.model/engine.
import type { ServiceContextProps } from '@holistic/ui';
import type { AskRequest, AskResult } from './types';

// Ask the room assistant. presentr's backend grounds the turn in the whole document pool and routes
// it through aigentic; a disabled/unavailable assistant surfaces as a rejected request the caller
// reports to the user.
export async function askRoom(api: ServiceContextProps['api'], req: AskRequest): Promise<AskResult> {
  return api.post<AskResult>('ask', {
    prompt: req.prompt,
    outputFormat: req.outputFormat ?? 'markdown',
    // The room chat passes the user's machine + model choice; the Connection diagram leaves both
    // unset and runs on Auto. Empty engine → "choose" on the backend (the Ask-AI default).
    engine: req.engine,
    model: req.model,
  });
}
