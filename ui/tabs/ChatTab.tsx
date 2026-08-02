import { useMemo } from 'react';
import {
  Badge,
  Chat,
  FileTextIcon,
  Stack,
  useLiveQuery,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import { makeRoomChatAdapter } from '../chatAdapter';
import type { DocsResponse } from '../types';

// The room assistant — the heart of presentr. It is NO LONGER a bespoke chat: it drives the ONE
// shared <Chat> building block from @holistic/ui (the same one aigentic uses) via a presentr
// ChatAdapter. The shared chat owns the whole experience — many conversations with
// switch/create/delete, machine+model choice, model-tagged answers, wrapping prose, a transcript
// and composer — so this tab is thin. presentr contributes only its OWN part: the answers are
// GROUNDED in the room's document pool (assembled on presentr's backend; see chatAdapter), and the
// composer shows how many room documents that grounding currently draws on.

// Reload-restore: the shared chat remembers the open conversation under this key, so a browser
// reload reopens the same one (Zustandserhalt). The active TAB is already restored from nav.path by
// the Dashboard, so together a reload lands the user on the same tab AND the same conversation.
const ACTIVE_KEY = 'presentr.room.chat.active';

export function ChatTab({ api, apiFor }: Pick<ServiceContextProps, 'api' | 'apiFor'>) {
  const t = useT();
  const docsQ = useLiveQuery<DocsResponse>(() => api.get<DocsResponse>('docs'), 15000);
  const adapter = useMemo(() => makeRoomChatAdapter(api, apiFor), [api, apiFor]);
  const docCount = docsQ.data?.docs?.length ?? 0;

  // presentr's own composer extra (kept out of the shared chat): a live indicator that answers are
  // grounded in the room's document pool, and in how many documents.
  const grounding = (
    <Stack direction="row" align="center" gap={1} className="shrink-0">
      <FileTextIcon className="h-4 w-4 text-text-tertiary" />
      <Badge variant="neutral">{docCount > 0 ? t('presentr.chatGrounded', { count: docCount }) : t('presentr.chatNoDocs')}</Badge>
    </Stack>
  );

  return (
    <Stack className="h-[72vh] min-h-[420px] rounded-md">
      <Chat adapter={adapter} persistKey={ACTIVE_KEY} composerAccessory={grounding} placeholder={t('presentr.chatPlaceholder')} />
    </Stack>
  );
}
