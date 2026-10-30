// Radix (popper, menus) reads browser APIs jsdom lacks; guarded so a node-env test is untouched.
if (typeof window !== 'undefined') {
  window.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} }
  Element.prototype.hasPointerCapture ??= () => false
  Element.prototype.setPointerCapture ??= () => {}
  Element.prototype.releasePointerCapture ??= () => {}
  Element.prototype.scrollIntoView ??= () => {}
}
