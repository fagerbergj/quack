// Resolves with the first frame matching the predicate; the deadline only matters when the code is broken.
export function nextFrame(ws: WebSocket, match: (frame: any) => boolean, ms = 15_000): Promise<any> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('no matching frame')), ms);
    ws.addEventListener('message', function onMessage(ev) {
      const frame = JSON.parse(String(ev.data));
      if (!match(frame)) return;
      clearTimeout(timer);
      ws.removeEventListener('message', onMessage);
      resolve(frame);
    });
  });
}

// Matches the view that shows a run over: no run, after a run was seen or an answer arrived. Items are sent only when
// they change, so the matching frame carries the latest as `items`.
export function runEnded() {
  let items: unknown[] = [];
  let ran = false;
  return (frame: any) => {
    if (frame.type !== 'view') return false;
    if (frame.items) items = frame.items;
    ran ||= frame.live.run !== undefined;
    if (frame.live.run !== undefined || !(ran || JSON.stringify(items).includes('"role":"assistant"'))) return false;
    frame.items = items;
    return true;
  };
}
