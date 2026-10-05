import { useEffect, useState } from 'react';
import type { LiveStatus } from '../live';
import { isStale } from '../live';

// LiveIndicator owns the 1s clock for staleness so the ticking re-renders only
// this badge, never the device list.
export function LiveIndicator({ live }: { live: LiveStatus }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  const stale = isStale(live, now);
  const age = live.lastUpdate === null ? null : Math.max(0, Math.round((now - live.lastUpdate) / 1000));

  let dot = 'bg-success';
  let label = 'Live';
  if (stale) {
    dot = 'bg-danger';
    label = age === null ? 'Stale: no data yet' : `Stale: last update ${age}s ago`;
  } else if (live.stream !== 'open') {
    dot = 'bg-warning';
    label = live.polling ? 'Reconnecting (polling every 5s)' : 'Connecting';
  }

  return (
    <div className="mb-4 flex items-center gap-2 text-xs text-fg-subtle" role="status" aria-live="polite">
      <span className={`inline-block h-2 w-2 rounded-full ${dot}`} />
      <span className={stale ? 'text-danger font-medium' : undefined}>{label}</span>
    </div>
  );
}
