// A pi package that touches UI while loading and keeps a resource until session_shutdown, as pi-lsp does.
export default function (pi: any) {
  pi.ui.notify('loading');
  pi.on('session_shutdown', async () => { (globalThis as any).piPkgStopped = true; });
  pi.registerTool({ name: 'echo_json', description: 'echo', parameters: { type: 'object', properties: {} }, async execute() { return { content: [{ type: 'text', text: '{"ok":true}' }] }; } });
}
