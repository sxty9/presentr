import { EmptyState, Panel, SendIcon, useT, type ServiceContextProps } from '@holisdk/ui';

// The room assistant — workflow stage 3, and the heart of presentr. The user asks questions and
// the assistant answers as an explainer, grounded in the document pool and the connection diagram.
// Per the holistic "Ask AI" standard this routes through the shared aigentic service, and every
// answer is labelled with the model that produced it. The live exchange is wired up in the next
// step; this placeholder states the contract so the tab reads honestly.
export function ChatTab(_props: Pick<ServiceContextProps, 'api' | 'apiFor' | 'user' | 'ui'>) {
  const t = useT();
  return (
    <Panel className="p-6">
      <EmptyState icon={<SendIcon />} title={t('presentr.chatSoonTitle')} description={t('presentr.chatSoon')} />
    </Panel>
  );
}
