// Theme resolution and persistence. index.html carries an inline copy of
// resolveTheme's logic so data-theme is set before first paint; keep the
// storage key and fallback rules in sync with it.

export type Theme = 'light' | 'dark';

export const STORAGE_KEY = 'kumaboard-theme';
export const DARK_QUERY = '(prefers-color-scheme: dark)';

type ReadStore = Pick<Storage, 'getItem'>;
type WriteStore = Pick<Storage, 'setItem'>;

export function isTheme(v: unknown): v is Theme {
  return v === 'light' || v === 'dark';
}

/** Explicit stored choice wins; otherwise follow the OS preference. */
export function resolveTheme(stored: string | null | undefined, prefersDark: boolean): Theme {
  if (isTheme(stored)) return stored;
  return prefersDark ? 'dark' : 'light';
}

/** Stored user choice, or null if unset/invalid/storage unavailable. */
export function readStoredTheme(store: ReadStore | undefined): Theme | null {
  try {
    const v = store?.getItem(STORAGE_KEY);
    return isTheme(v) ? v : null;
  } catch {
    return null;
  }
}

export function persistTheme(store: WriteStore | undefined, theme: Theme): void {
  try {
    store?.setItem(STORAGE_KEY, theme);
  } catch {
    /* storage disabled (private mode, quota): choice lasts for this page only */
  }
}

export function toggleTheme(theme: Theme): Theme {
  return theme === 'dark' ? 'light' : 'dark';
}

export function applyTheme(root: HTMLElement, theme: Theme): void {
  root.dataset.theme = theme;
}

export function systemPrefersDark(): boolean {
  return typeof window !== 'undefined' && typeof window.matchMedia === 'function'
    && window.matchMedia(DARK_QUERY).matches;
}

export function currentTheme(): Theme {
  const stored = typeof window === 'undefined' ? null : readStoredTheme(window.localStorage);
  return resolveTheme(stored, systemPrefersDark());
}
