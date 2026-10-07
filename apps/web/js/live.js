// Live updates over the server-sent event stream (GET /api/events). The
// browser's EventSource reconnects on its own and sends Last-Event-ID, so
// changes made while disconnected are replayed by the server.

export function connectLive({ onReady, onChange, onResync, onStatus }) {
  if (typeof EventSource === 'undefined') {
    onStatus('unsupported');
    return () => {};
  }
  const es = new EventSource('/api/events');
  onStatus('connecting');
  const parse = (e) => {
    try {
      return JSON.parse(e.data);
    } catch {
      return null;
    }
  };
  es.addEventListener('ready', (e) => {
    onStatus('live');
    onReady(parse(e));
  });
  es.addEventListener('task', (e) => {
    const ev = parse(e);
    if (ev) onChange(ev);
  });
  es.addEventListener('resync', () => onResync());
  es.addEventListener('error', () => {
    onStatus(es.readyState === EventSource.CLOSED ? 'closed' : 'offline');
  });
  return () => es.close();
}
