// Messages for presentr, authored in English (US) — the canonical source. Registered on import
// (see index.tsx); the holistic SDK owns the i18n machinery (registerMessages + useT), so presentr
// consumes one shared engine instead of hardcoding strings. Other locales are added downstream by
// the nightly translation run. The shell shows the localized `service.presentr` label in the
// sidebar, falling back to the plugin's displayName.
import { registerMessages } from '@holisdk/ui';

registerMessages({
  'en-US': {
    'service.presentr': 'Presentr',
    'presentr.title': 'Presentr',

    // Access gate (shown when the caller lacks the right)
    'presentr.needRight':
      'You need the “Use presentr” right. An admin can grant it per user in the Rights (privleg) service.',

    // Tabs
    'presentr.tabDocs': 'Docs',
    'presentr.tabConnection': 'Connection',
    'presentr.tabChat': 'Chat',

    // Docs tab
    'presentr.docsHeading': 'Document pool',
    'presentr.docsSubtitle': 'Everything known about the room — manuals, notes, the layout.',
    'presentr.addDoc': 'Add text',
    'presentr.noDocs': 'No documents yet. Add what you know about the room.',
    'presentr.selectDoc': 'Select a document to read it.',
    'presentr.byAuthor': 'Added by {author}',
    'presentr.docTitle': 'Title',
    'presentr.docTitlePlaceholder': 'e.g. Projector — Epson EB-2250U',
    'presentr.docDescription': 'Short description',
    'presentr.docDescriptionPlaceholder': 'One line: what is this?',
    'presentr.docContent': 'Content',
    'presentr.docContentPlaceholder': 'Paste the manual, notes or layout here. Markdown is supported.',
    'presentr.save': 'Save',
    'presentr.cancel': 'Cancel',
    'presentr.saveFailed': 'Could not save the document',
    'presentr.delete': 'Delete',
    'presentr.deleteTitle': 'Delete document',
    'presentr.deleteConfirm': 'Remove this document from the room pool? This cannot be undone.',
    'presentr.deleteFailed': 'Could not delete the document',

    // Connection tab (the auto-generated wiring diagram — generation lands next)
    'presentr.diagramSoonTitle': 'Connection diagram',
    'presentr.diagramSoon':
      'The wiring diagram is derived from the {count} documents in the pool. Automatic generation is being built.',

    // Chat tab (the assistant — routed through aigentic)
    'presentr.chatHeading': 'Room assistant',
    'presentr.chatSubtitle': 'Grounded in the document pool. Answers are labelled with the model that produced them.',
    'presentr.chatPlaceholder': 'Ask about the room — e.g. “How do I connect my laptop to the projector?”',
    'presentr.chatSend': 'Send',
    'presentr.chatThinking': 'Thinking…',
    'presentr.chatEmpty': '(no answer)',
    'presentr.chatFailed': 'The assistant could not answer',
    'presentr.chatEmptyTitle': 'Ask the room assistant',
    'presentr.chatEmptyBody': 'Ask anything about the room. Answers are grounded in the documents you added in the Docs tab.',
    'presentr.chatClear': 'Clear',
    'presentr.chatClearTitle': 'Clear conversation',
    'presentr.chatClearConfirm': 'Delete this conversation? This cannot be undone.',
  },
});
