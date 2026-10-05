// Hook that keeps dashboard state live via LiveSync (SSE + poll fallback).

import { useEffect, useState } from 'react';
import type { Device } from './api';
import { api, ApiError } from './api';
import type { State } from './state';
import { emptyState, loadDevices, reduce } from './state';
import type { LiveStatus } from './live';
import { initialStatus, LiveSync } from './live';

export function useEvents(): { state: State; live: LiveStatus } {
  const [state, setState] = useState<State>(emptyState);
  const [live, setLive] = useState<LiveStatus>(initialStatus);

  useEffect(() => {
    const sync = new LiveSync({
      openStream: () => new EventSource('/api/events'),
      fetchDevices: () => api<Device[]>('/api/devices'),
      onEvent: (e) => setState((prev) => reduce(prev, e)),
      onDevices: (d) => setState((prev) => loadDevices(prev, d)),
      onStatus: (s) => {
        setLive(s);
        if (s.unauthenticated) setState(emptyState);
      },
      isAuthError: (err) => err instanceof ApiError && err.status === 401,
    });
    sync.start();
    return () => sync.stop();
  }, []);

  return { state, live };
}
