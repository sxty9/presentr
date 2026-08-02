// Messages for presentr, authored in English (US) — the canonical source. Registered on import
// (see index.tsx); the holistic SDK owns the i18n machinery (registerMessages + useT), so presentr
// consumes one shared engine instead of hardcoding strings. Other locales are added downstream by
// the nightly translation run. The shell shows the localized `service.presentr` label in the
// sidebar, falling back to the plugin's displayName.
import { registerMessages } from '@holistic/ui';

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
    'presentr.docsSubtitle': 'Everything known about the room — manuals, photos, notes, the layout.',
    'presentr.addText': 'Write text',
    'presentr.uploadFiles': 'Upload files',
    'presentr.openFile': 'Open',
    'presentr.dropHint': 'Drop files to add them to the room.',
    'presentr.uploadHint': 'Images, PDFs, text — up to {limit} MB each',
    'presentr.uploadTooBigItem': '{name} ({size}) is over the {limit} MB limit',
    // Per-file upload progress (each file uploads on its own, with its own bar and outcome)
    'presentr.uploadsHeading': 'Uploads',
    'presentr.uploadClearDone': 'Clear finished',
    'presentr.uploadCancel': 'Cancel upload',
    'presentr.uploadDismiss': 'Dismiss',
    'presentr.uploadState_queued': 'Queued',
    'presentr.uploadState_uploading': 'Uploading',
    'presentr.uploadState_done': 'Added',
    'presentr.uploadState_rejected': 'Rejected',
    'presentr.uploadState_canceled': 'Canceled',
    'presentr.uploadState_error': 'Failed',
    'presentr.noDocs': 'No documents yet. Add what you know about the room — text, PDFs, photos.',
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

    // Text extraction — the text read once out of an uploaded file (a PDF's text layer, or the text on
    // a photo/scan, recognized by the assistant), kept beside the file and used to answer questions.
    'presentr.extractReading': 'Reading text…',
    'presentr.extractReadingSections': 'Reading — section {done} of {total}',
    'presentr.extractUnread': 'Text not read',
    'presentr.extractReadHeading': 'Text read from this file',
    'presentr.extractFromLayer': 'Read from the document’s text layer — exact, no AI used.',
    'presentr.extractFromAI': 'Text recognized by the assistant (including text on photos and scans), read by {model}.',
    'presentr.extractPendingBody': 'Reading the text out of this file. This runs once; you can keep working.',
    'presentr.extractReadingBody': 'This file is large, so it is read in {total} sections. Reading section {done} of {total} — you can keep working; it continues where it left off if interrupted.',
    'presentr.extractFailedBody': 'The text of this file could not be read. {reason}',
    'presentr.extractRetry': 'Read again',
    'presentr.extractRetryFailed': 'Could not start reading the file again',
    'presentr.extractShow': 'Show the read text',
    'presentr.extractHide': 'Hide the read text',
    'presentr.extractEmpty': 'No legible text was found in this file.',
    'presentr.extractLoadFailed': 'Could not load the read text',

    // Connection tab (the wiring diagram, derived from the documents and editable by hand)
    'presentr.diagramHeading': 'Connection diagram',
    'presentr.stateDocument': 'Document state',
    'presentr.stateManual': 'Manually modified',
    'presentr.restore': 'Restore document state',
    'presentr.addDevice': 'Add device',
    'presentr.generate': 'Generate from documents',
    'presentr.diagramEmptyTitle': 'No diagram yet',
    'presentr.diagramEmptyBody': 'Generate the diagram from your documents, or add devices by hand and connect their ports.',
    'presentr.ports': 'ports',
    'presentr.deleteDevice': 'Delete device',
    'presentr.deleteConnection': 'Delete connection',
    'presentr.connectHint': 'Now click a port on another device to connect them.',
    'presentr.deviceName': 'Name',
    'presentr.deviceNamePlaceholder': 'e.g. Projector',
    'presentr.deviceSymbol': 'Symbol',
    'presentr.devicePorts': 'Number of ports',
    'presentr.add': 'Add',
    'presentr.diagramNeedDocs': 'Add some documents first — the diagram is derived from them.',
    'presentr.diagramNoResult': 'The assistant could not derive a diagram from the documents.',
    'presentr.diagramGenFailed': 'Could not generate the diagram',
    'presentr.diagramSaveFailed': 'Could not save the diagram',
    'presentr.diagramRestoreFailed': 'Could not restore the diagram',

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
