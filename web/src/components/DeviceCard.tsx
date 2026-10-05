import { Link } from 'react-router';
import type { Device } from '../api';
import { bytes, uptime, when } from '../format';
import { gpuCardLines } from '../gpu';
import { tempClass, tempText } from '../temp';
import { HistorySparklines } from './HistorySparklines';
import { StateBadge } from './StateBadge';

interface Props {
  device: Device;
  onWake?: (name: string) => void;
}

export function DeviceCard({ device, onWake }: Props) {
  const m = device.metrics;
  return (
    <div className="rounded-lg border border-border bg-surface p-4 shadow-sm">
      <div className="flex items-center justify-between mb-2">
        <Link to={`/devices/${device.name}`} className="text-lg font-semibold text-fg hover:underline">{device.name}</Link>
        <StateBadge state={device.state} />
      </div>

      {/* The card body also opens the detail page; the Wake button stays outside it. */}
      <Link to={`/devices/${device.name}`} className="block">
        <div className="text-sm text-fg-subtle space-y-1">
          <p>{device.os}/{device.arch} &middot; v{device.agent_version}</p>
          <p>Last seen: {when(device.last_seen)}</p>
          {device.incompatible && (
            <p className="text-danger font-medium">Incompatible: {device.reject_reason}</p>
          )}
        </div>

        {m && (
          <div className="mt-3 grid grid-cols-2 gap-2 text-xs text-fg-muted">
            <div>CPU: {m.cpu_percent.toFixed(1)}%</div>
            <div>Mem: {bytes(m.mem_used_bytes)} / {bytes(m.mem_total_bytes)}</div>
            {m.temp_c !== undefined && (
              <div title={m.temp_sensor || undefined}>
                Temp: <span className={tempClass(m.temp_c) || undefined}>{tempText(m.temp_c)}</span>
              </div>
            )}
            <div>Disk: {bytes(m.disk_used_bytes)} / {bytes(m.disk_total_bytes)}</div>
            <div>Up: {uptime(m.uptime_s)}</div>
            <div>Load: {m.load1.toFixed(2)} / {m.load5.toFixed(2)} / {m.load15.toFixed(2)}</div>
            {gpuCardLines(m.gpus).map((line) => <div key={line}>{line}</div>)}
          </div>
        )}

        {device.connected && <HistorySparklines name={device.name} live={m} />}
      </Link>

      {device.mac && !device.connected && onWake && (
        <button
          onClick={() => onWake(device.name)}
          className="mt-3 w-full rounded bg-accent px-3 py-1.5 text-sm text-on-accent hover:bg-accent-hover"
        >
          Wake
        </button>
      )}
    </div>
  );
}
