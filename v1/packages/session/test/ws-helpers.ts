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

export const runEnded = (frame: any) => frame.type === 'events' && frame.events.some((e: any) => e.type === 'run_end');
