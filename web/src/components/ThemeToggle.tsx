import { useEffect, useState } from 'react';
import {
  DARK_QUERY,
  type Theme,
  applyTheme,
  currentTheme,
  persistTheme,
  readStoredTheme,
  resolveTheme,
  toggleTheme,
} from '../theme';

export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(currentTheme);

  useEffect(() => {
    applyTheme(document.documentElement, theme);
  }, [theme]);

  // Follow OS changes until the user makes an explicit choice.
  useEffect(() => {
    const mq = window.matchMedia?.(DARK_QUERY);
    if (!mq) return;
    const onChange = (e: MediaQueryListEvent) => {
      if (readStoredTheme(window.localStorage) === null) setTheme(resolveTheme(null, e.matches));
    };
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, []);

  const next = toggleTheme(theme);
  const flip = () => {
    persistTheme(window.localStorage, next);
    setTheme(next);
  };

  return (
    <button
      type="button"
      onClick={flip}
      aria-label={`Switch to ${next} theme`}
      title={`Switch to ${next} theme`}
      className="rounded p-1.5 text-fg-muted hover:bg-surface-muted hover:text-fg"
    >
      {theme === 'dark' ? (
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41" />
        </svg>
      ) : (
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
        </svg>
      )}
    </button>
  );
}
