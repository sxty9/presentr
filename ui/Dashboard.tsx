import {
  FileTextIcon,
  NetworkIcon,
  Panel,
  SegmentedControl,
  SendIcon,
  Stack,
  Text,
  useT,
  userHasRight,
  type SegmentedOption,
  type ServiceContextProps,
} from '@holistic/ui';
import { DocsTab } from './tabs/DocsTab';
import { ConnectionTab } from './tabs/ConnectionTab';
import { ChatTab } from './tabs/ChatTab';

// Mirrors permissions/presentr.json → room:use and internal/rights.GroupUse on the backend. The
// shell resolves groups from Linux; admins always pass. Keep all three in sync.
const USE_RIGHT = 'hp_presentr_use';

// The three tabs from the sketches: the document pool, the derived connection diagram, and the
// assistant. The active tab lives in the service sub-path (nav.path), so a browser reload lands
// the user back on the same tab (Zustandserhalt beim Reload).
const TAB_IDS = ['docs', 'connection', 'chat'] as const;
type TabId = (typeof TAB_IDS)[number];

function tabFromPath(path: string): TabId {
  const seg = (path || '').replace(/^\/+/, '').split('/')[0];
  return (TAB_IDS as readonly string[]).includes(seg) ? (seg as TabId) : 'docs';
}

export function Dashboard({ user, api, nav, ui }: ServiceContextProps) {
  const t = useT();

  // Gate the whole surface on the one right, exactly as the backend does. A user without it
  // normally never sees the tab at all (the shell's default gate), but an admin viewing another
  // account, or a direct link, still resolves here — so we fail closed in the UI too.
  if (!userHasRight(user, USE_RIGHT)) {
    return (
      <Panel title={t('presentr.title')} className="p-4">
        <Text color="secondary">{t('presentr.needRight')}</Text>
      </Panel>
    );
  }

  const active = tabFromPath(nav.path);
  const options: SegmentedOption<TabId>[] = [
    { value: 'docs', label: t('presentr.tabDocs'), icon: <FileTextIcon /> },
    { value: 'connection', label: t('presentr.tabConnection'), icon: <NetworkIcon /> },
    { value: 'chat', label: t('presentr.tabChat'), icon: <SendIcon /> },
  ];

  return (
    <Stack gap={4}>
      <SegmentedControl options={options} value={active} onChange={(v) => nav.navigate(v)} />
      {active === 'docs' && <DocsTab api={api} ui={ui} />}
      {active === 'connection' && <ConnectionTab api={api} ui={ui} />}
      {active === 'chat' && <ChatTab api={api} ui={ui} />}
    </Stack>
  );
}
