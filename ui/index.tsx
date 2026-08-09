import { NetworkIcon, type ServicePlugin } from '@holisdk/ui';
import { Dashboard } from './Dashboard';
import './i18n';

// presentr's dashboard plugin. Linked into holistic/frontend/external/presentr at install time
// and discovered by the host SPA's build-time registry. `id` MUST equal the link dir name and the
// permissions manifest's "service" field. `displayName` is the static plugin name; the shell
// prefers the localized `service.presentr` message (registered in ./i18n). No `visible` gate is
// needed: presentr's right follows the hp_<id>_* convention, so the shell's default gate already
// hides the tab from users without it (Rechtelose Dienste verborgen).
const plugin: ServicePlugin = {
  id: 'presentr',
  displayName: 'Presentr',
  icon: NetworkIcon,
  order: 100,
  Component: Dashboard,
};

export default plugin;
