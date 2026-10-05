// Host temperature display helpers. Thresholds are fleet-wide constants: 80 °C
// is where Pi firmware soft-throttles, 95 °C where x86 parts hit their own
// limit. An absent reading is never shown (no fake zero, no placeholder).

export const TEMP_WARN_C = 80;
export const TEMP_CRIT_C = 95;

// The y-axis maximum for temperature plots, so a crit reading isn't pinned to
// the top edge.
export const TEMP_SCALE_MAX_C = 110;

export type TempLevel = 'ok' | 'warn' | 'crit';

export function tempLevel(c: number): TempLevel {
  if (c >= TEMP_CRIT_C) return 'crit';
  if (c >= TEMP_WARN_C) return 'warn';
  return 'ok';
}

// tempText renders a reading as whole degrees, e.g. "54°C".
export function tempText(c: number): string {
  return `${c.toFixed(0)}°C`;
}

const CLASSES: Record<TempLevel, string> = {
  ok: '',
  warn: 'text-warning',
  crit: 'text-danger font-medium',
};

// tempClass is the text colour for a reading: none, warning or danger.
export function tempClass(c: number): string {
  return CLASSES[tempLevel(c)];
}
