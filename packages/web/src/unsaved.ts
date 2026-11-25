const TEXT = new Set(['text', 'search', 'url', 'email', 'number', 'textarea']);

/** True when a reload would lose something: a typed field, an open dialog, or anything marked data-unsaved (pending attachments). */
export function unsaved(doc: Document = document) {
  if (doc.querySelector('[role="dialog"], [data-unsaved]')) return true;
  return [...doc.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('input, textarea')].some((el) => TEXT.has(el.type) && el.value.trim() !== '');
}
