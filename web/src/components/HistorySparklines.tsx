import { useEffect, useState } from 'react';
import { api, type HistorySample, type Metrics, type MetricsHistory } from '../api';
import { BUCKET_S, historySeries, mergeLive } from '../history';
import { TEMP_SCALE_MAX_C, tempClass } from '../temp';
import { Sparkline } from './Sparkline';

interface Props {
  name: string;
  live: Metrics | null;
}

// HistorySparklines shows the last hour of CPU/RAM/temperature/GPU for a connected
// device.
// History is refetched once per bucket; live SSE samples extend the tail.
export function HistorySparklines({ name, live }: Props) {
  const [samples, setSamples] = useState<HistorySample[]>([]);

  useEffect(() => {
    let cancelled = false;
    const load = () => {
      api<MetricsHistory>(`/api/devices/${name}/metrics/history?window=1h`)
        .then((h) => { if (!cancelled) setSamples(h.samples); })
        .catch(() => {});
    };
    load();
    const id = setInterval(load, BUCKET_S * 1000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [name]);

  const series = historySeries(mergeLive(samples, live, Math.floor(Date.now() / 1000)));
  // Sparkline has no gaps, so missing temperatures collapse out of the line.
  const temp = series.temp?.filter((v): v is number => v !== null);
  return (
    <div className="mt-3 space-y-1">
      <Sparkline label="CPU 1h" values={series.cpu} color="var(--color-chart-1)" />
      <Sparkline label="RAM 1h" values={series.mem} color="var(--color-chart-2)" />
      {temp && temp.length > 0 && (
        <Sparkline
          label="Temp 1h"
          values={temp}
          max={TEMP_SCALE_MAX_C}
          unit="°C"
          valueClass={tempClass(temp[temp.length - 1])}
          color="var(--color-chart-3)"
        />
      )}
      {series.gpu && <Sparkline label="GPU 1h" values={series.gpu} color="var(--color-chart-4)" />}
    </div>
  );
}
