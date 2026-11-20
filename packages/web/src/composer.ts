import type { KeyboardEvent } from 'react';

const isCoarsePointer = () => typeof matchMedia === 'function' && matchMedia('(pointer: coarse)').matches;

// Plain Enter submits on fine pointers; Shift+Enter and Ctrl+Enter insert newlines; IME composition is ignored.
export function composerKeyDown(e: KeyboardEvent<HTMLTextAreaElement>, setValue: (v: string) => void) {
  if (e.nativeEvent.isComposing || e.key !== 'Enter' || e.shiftKey || e.metaKey || e.altKey) return;
  if (e.ctrlKey) {
    e.preventDefault();
    const el = e.currentTarget;
    const start = el.selectionStart ?? el.value.length;
    const end = el.selectionEnd ?? start;
    setValue(`${el.value.slice(0, start)}\n${el.value.slice(end)}`);
    queueMicrotask(() => el.isConnected && el.setSelectionRange(start + 1, start + 1));
    return;
  }
  if (!isCoarsePointer()) {
    e.preventDefault();
    e.currentTarget.form?.requestSubmit();
  }
}
