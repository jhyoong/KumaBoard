// Central handling of unauthenticated API responses. api() reports every 401
// here; the first one sends the SPA to /login (carrying the current path as
// ?next=) and later ones are dropped until the login page mounts and re-arms
// the guard, so concurrent failing calls produce exactly one redirect.

export const LOGIN_PATH = '/login';

export interface AuthNavigator {
  navigate: (to: string) => void;
  // Current path + search, used as the post-login destination.
  currentPath: () => string;
}

let nav: AuthNavigator | null = null;
let redirecting = false;

// setAuthNavigator installs the router hook-up; returns an uninstall func.
export function setAuthNavigator(n: AuthNavigator): () => void {
  nav = n;
  return () => {
    if (nav === n) nav = null;
  };
}

// resetAuthRedirect re-arms the redirect guard; the login page calls it once
// the redirect has landed.
export function resetAuthRedirect(): void {
  redirecting = false;
}

// handleUnauthorized sends the SPA to /login once. It is a no-op while
// already on /login so the login page never redirects to itself.
export function handleUnauthorized(): void {
  if (redirecting || !nav) return;
  const path = nav.currentPath();
  if (isLoginPath(path)) return;
  redirecting = true;
  nav.navigate(loginURL(path));
}

function isLoginPath(path: string): boolean {
  return path === LOGIN_PATH || path.startsWith(LOGIN_PATH + '?') || path.startsWith(LOGIN_PATH + '/');
}

// loginURL builds /login?next=<path>, omitting next for the root.
export function loginURL(path: string): string {
  const next = safeNext(path);
  return next === '/' ? LOGIN_PATH : `${LOGIN_PATH}?next=${encodeURIComponent(next)}`;
}

// safeNext returns next if it is a same-origin app path, else '/'. Rejects
// absolute and protocol-relative URLs (open redirect) and /login itself.
export function safeNext(next: string | null | undefined): string {
  if (!next || !next.startsWith('/') || next.startsWith('//') || next.startsWith('/\\')) return '/';
  if (isLoginPath(next)) return '/';
  return next;
}
