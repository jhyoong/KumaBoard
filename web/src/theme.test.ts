import { describe, it, expect } from 'vitest';
import {
  STORAGE_KEY,
  applyTheme,
  isTheme,
  persistTheme,
  readStoredTheme,
  resolveTheme,
  toggleTheme,
} from './theme';

function memStore(init: Record<string, string> = {}) {
  const data = new Map(Object.entries(init));
  return {
    data,
    getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => void data.set(k, v),
  };
}

const throwing = {
  getItem: () => { throw new Error('SecurityError'); },
  setItem: () => { throw new Error('QuotaExceededError'); },
};

describe('resolveTheme', () => {
  it('defaults to prefers-color-scheme when nothing is stored', () => {
    expect(resolveTheme(null, true)).toBe('dark');
    expect(resolveTheme(null, false)).toBe('light');
    expect(resolveTheme(undefined, true)).toBe('dark');
  });

  it('stored choice overrides the system preference', () => {
    expect(resolveTheme('light', true)).toBe('light');
    expect(resolveTheme('dark', false)).toBe('dark');
  });

  it('ignores invalid stored values', () => {
    expect(resolveTheme('purple', true)).toBe('dark');
    expect(resolveTheme('', false)).toBe('light');
  });
});

describe('storage', () => {
  it('round-trips through localStorage under the kumaboard-theme key', () => {
    const s = memStore();
    expect(readStoredTheme(s)).toBeNull();
    persistTheme(s, 'dark');
    expect(s.data.get(STORAGE_KEY)).toBe('dark');
    expect(STORAGE_KEY).toBe('kumaboard-theme');
    expect(readStoredTheme(s)).toBe('dark');
    persistTheme(s, 'light');
    expect(readStoredTheme(s)).toBe('light');
  });

  it('persisted choice beats the OS default on next load', () => {
    const s = memStore();
    persistTheme(s, 'light');
    expect(resolveTheme(readStoredTheme(s), true)).toBe('light');
  });

  it('treats garbage as unset', () => {
    expect(readStoredTheme(memStore({ [STORAGE_KEY]: 'blue' }))).toBeNull();
  });

  it('survives unavailable or throwing storage', () => {
    expect(readStoredTheme(undefined)).toBeNull();
    expect(readStoredTheme(throwing)).toBeNull();
    expect(() => persistTheme(throwing, 'dark')).not.toThrow();
    expect(() => persistTheme(undefined, 'dark')).not.toThrow();
  });
});

describe('helpers', () => {
  it('toggleTheme flips', () => {
    expect(toggleTheme('light')).toBe('dark');
    expect(toggleTheme('dark')).toBe('light');
  });

  it('isTheme narrows', () => {
    expect(isTheme('dark')).toBe(true);
    expect(isTheme(null)).toBe(false);
  });

  it('applyTheme sets data-theme', () => {
    const el = { dataset: {} as Record<string, string> } as unknown as HTMLElement;
    applyTheme(el, 'dark');
    expect(el.dataset.theme).toBe('dark');
  });
});
