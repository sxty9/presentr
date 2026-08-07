// The room's AI access point. presentr asks the room's assistant two things — "explain the room"
// (Chat) and "derive the wiring from the documents" (Connection) — and both must ground the model in
// the SAME source: the room's document pool. That grounding now lives on presentr's BACKEND (POST
// ask), because the pool holds uploaded PDFs and images whose bytes aigentic reads natively:
// assembling the grounding server-side keeps those bytes off the wire to the browser and back
// (Portionierte Daten) and keeps ONE access point to the room AI. So this module is a thin client of
// that endpoint — the Chat tab and the Connection diagram differ only in their prompt and requested
// output shape. Every answer carries the model that produced it; the caller labels its bubble/turn
// from AskResult.model/engine.
//
// The turn runs as a BACKGROUND job on the backend (a grounded turn over a large pool can take longer
// than the edge proxy's ~100s limit, which would return a raw 524 page instead of an answer). So askRoom
// starts the job, then POLLS its status, reporting granular progress to the caller until the answer is
// ready — the Chat tab and the Connection diagram show these steps instead of a bare spinner.
import type { ServiceContextProps } from '@holistic/ui';
import type { AskJob, AskJobStart, AskProgress, AskRequest, AskResult } from './types';

// pollIntervalMs is how often the UI asks the backend how the turn is going. Short enough that each step
// (context assembled, sent, answer received) shows promptly; a status poll returns immediately, so it
// never itself risks the edge timeout the background job exists to avoid.
const pollIntervalMs = 1000;

// maxPollErrors is how many consecutive status polls may fail (a transient network blip) before askRoom
// gives up — so one dropped poll does not throw away a long answer that is still being produced.
const maxPollErrors = 4;

const delay = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

// askRoom starts the background turn and resolves with its answer, calling onProgress with each step as
// the backend reports it. A disabled/unavailable assistant, or a failed turn, surfaces as a rejected
// promise the caller reports to the user (see describeAskError for the toast-safe message).
export async function askRoom(
  api: ServiceContextProps['api'],
  req: AskRequest,
  onProgress?: (p: AskProgress) => void,
): Promise<AskResult> {
  const start = await api.post<AskJobStart>('ask', {
    prompt: req.prompt,
    outputFormat: req.outputFormat ?? 'markdown',
  });
  onProgress?.({ phase: start.phase || 'grounding', docs: 0, images: 0 });

  let errors = 0;
  for (;;) {
    await delay(pollIntervalMs);
    let job: AskJob;
    try {
      job = await api.get<AskJob>(`ask/${start.jobId}`);
      errors = 0;
    } catch (e) {
      if (++errors >= maxPollErrors) throw e;
      continue;
    }
    onProgress?.({ phase: job.phase, docs: job.docs, images: job.images });
    if (job.state === 'done') {
      return { output: job.output ?? '', model: job.model, engine: job.engine };
    }
    if (job.state === 'failed') {
      throw new Error(job.error || 'The assistant could not answer');
    }
  }
}

// askStepLabel maps one progress report to the localized step the UI shows. The three steps mirror the
// backend's phases: gathering the room context, sending it to the assistant (with what it carried), and
// the answer arriving.
export function askStepLabel(
  t: (key: string, params?: Record<string, string | number>) => string,
  p: AskProgress | null,
): string {
  switch (p?.phase) {
    case 'sending':
      return t('presentr.askStepSending', { docs: p.docs, images: p.images });
    case 'done':
      return t('presentr.askStepReceived');
    case 'grounding':
    default:
      return t('presentr.askStepGrounding');
  }
}

// describeAskError turns an unknown thrown value into a message safe to show in a toast. The room AI is
// reached through the edge proxy, which on a timeout returns a raw HTML error page (a Cloudflare 524),
// not the backend's JSON {detail}. Dumping that HTML into a toast is unreadable, so a body that is not a
// parsed API message (it looks like HTML or a proxy error) is replaced with a short, generic sentence.
export function describeAskError(
  t: (key: string, params?: Record<string, string | number>) => string,
  e: unknown,
): string {
  const raw = e instanceof Error ? e.message : typeof e === 'string' ? e : '';
  const msg = raw.trim();
  if (!msg) return t('presentr.serverError');
  if (/^</.test(msg) || /<!doctype|<html|<head|<body|cloudflare|error code\s*\d/i.test(msg)) {
    return t('presentr.serverTimeout');
  }
  return msg;
}
