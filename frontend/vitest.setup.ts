// jsdom has no modal <dialog> or popover API. These stubs only toggle `open` and fire `close`/`toggle`; Esc,
// light dismiss, the focus trap and native focus restore are browser behaviour, covered by render-check.
if (typeof HTMLDialogElement !== 'undefined' && !HTMLDialogElement.prototype.showModal) {
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.setAttribute('open', '') }
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
    if (!this.open) return
    this.removeAttribute('open')
    this.dispatchEvent(new Event('close'))
    this.dispatchEvent(Object.assign(new Event('toggle'), { newState: 'closed', oldState: 'open' }))
  }
  HTMLElement.prototype.showPopover = function (this: HTMLElement) { this.setAttribute('open', '') }
  HTMLElement.prototype.hidePopover = function (this: HTMLElement) {
    this.removeAttribute('open')
    this.dispatchEvent(Object.assign(new Event('toggle'), { newState: 'closed', oldState: 'open' }))
  }
}

// Radix (popper, menus) reads browser APIs jsdom lacks; guarded so a node-env test is untouched.
if (typeof window !== 'undefined') {
  window.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} }
  Element.prototype.hasPointerCapture ??= () => false
  Element.prototype.setPointerCapture ??= () => {}
  Element.prototype.releasePointerCapture ??= () => {}
  Element.prototype.scrollIntoView ??= () => {}
}
