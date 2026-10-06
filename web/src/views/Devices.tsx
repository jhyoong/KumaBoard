import { DeviceCard } from '../components/DeviceCard';
import { LiveIndicator } from '../components/LiveIndicator';
import type { LiveStatus } from '../live';
import type { State } from '../state';

interface Props {
  state: State;
  live: LiveStatus;
  onWake: (name: string) => void;
}

export function Devices({ state, live, onWake }: Props) {
  const devices = Object.values(state.devices);

  return (
    <div>
      <LiveIndicator live={live} />
      {live.error && (
        <div className="mb-4 rounded bg-danger-soft p-3 text-sm text-danger">{live.error}</div>
      )}

      {live.unauthenticated ? (
        <p className="text-fg-subtle text-center py-12">Not signed in. Redirecting to login...</p>
      ) : devices.length === 0 ? (
        <p className="text-fg-subtle text-center py-12">
          {live.lastUpdate === null ? 'Loading devices...' : 'No devices registered yet.'}
        </p>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-[repeat(auto-fill,minmax(20rem,1fr))]">
          {devices.map((d) => (
            <DeviceCard key={d.name} device={d} onWake={onWake} />
          ))}
        </div>
      )}
    </div>
  );
}
