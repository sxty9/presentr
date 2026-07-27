import { EmptyState, NetworkIcon, Panel, useLiveQuery, useT, type ServiceContextProps } from '@holisdk/ui';
import type { DocsResponse } from '../types';

// The connection diagram — workflow stage 2. It is derived from the document pool: an AI
// investigation of the documents yields the devices and how they are wired, which the user can
// then modify by hand (Document state ⇄ Manually modified). The generator and the editable canvas
// land in a following step; for now this reflects the live pool the diagram will be built from.
export function ConnectionTab({ api }: Pick<ServiceContextProps, 'api'>) {
  const t = useT();
  const docsQ = useLiveQuery<DocsResponse>(() => api.get<DocsResponse>('docs'), 8000);
  const count = docsQ.data?.docs?.length ?? 0;

  return (
    <Panel className="p-6">
      <EmptyState icon={<NetworkIcon />} title={t('presentr.diagramSoonTitle')} description={t('presentr.diagramSoon', { count })} />
    </Panel>
  );
}
