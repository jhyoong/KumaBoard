import type { DeviceState } from '../api';

const styles: Record<DeviceState, string> = {
  online: 'bg-success-soft text-success',
  stale: 'bg-warning-soft text-warning',
  offline_expected: 'bg-surface-muted text-fg-muted',
  offline_unexpected: 'bg-danger-soft text-danger',
};

const labels: Record<DeviceState, string> = {
  online: 'Online',
  stale: 'Stale',
  offline_expected: 'Offline (expected)',
  offline_unexpected: 'Offline',
};

export function StateBadge({ state }: { state: DeviceState }) {
  return (
    <span className={`inline-block rounded-full px-2 py-0.5 text-xs font-medium ${styles[state]}`}>
      {labels[state]}
    </span>
  );
}
