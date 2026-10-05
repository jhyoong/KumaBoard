import { type FormEvent, useEffect, useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { api, ApiError } from '../api';
import { resetAuthRedirect, safeNext } from '../auth';

export function Login() {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const navigate = useNavigate();
  const [params] = useSearchParams();

  useEffect(resetAuthRedirect, []);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError('');
    setLoading(true);
    try {
      await api('/api/login', {
        method: 'POST',
        body: JSON.stringify({ username, password }),
      });
      navigate(safeNext(params.get('next')), { replace: true });
    } catch (err) {
      if (err instanceof ApiError) {
        setError(err.message);
      } else {
        setError('Login failed');
      }
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-canvas">
      <form
        onSubmit={handleSubmit}
        className="w-full max-w-sm rounded-lg border border-border bg-surface p-8 shadow-md"
      >
        <h1 className="mb-6 text-2xl font-bold text-fg text-center">KumaBoard</h1>

        {error && (
          <div className="mb-4 rounded bg-danger-soft p-3 text-sm text-danger">{error}</div>
        )}

        <label className="block mb-4">
          <span className="block text-sm font-medium text-fg-muted mb-1">Username</span>
          <input
            type="text"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            required
            autoFocus
            className="w-full rounded border border-border-strong bg-inset px-3 py-2 text-sm focus:border-focus focus:outline-none focus:ring-1 focus:ring-focus"
          />
        </label>

        <label className="block mb-6">
          <span className="block text-sm font-medium text-fg-muted mb-1">Password</span>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            className="w-full rounded border border-border-strong bg-inset px-3 py-2 text-sm focus:border-focus focus:outline-none focus:ring-1 focus:ring-focus"
          />
        </label>

        <button
          type="submit"
          disabled={loading}
          className="w-full rounded bg-accent px-4 py-2 text-sm font-medium text-on-accent hover:bg-accent-hover disabled:opacity-50"
        >
          {loading ? 'Signing in...' : 'Sign in'}
        </button>
      </form>
    </div>
  );
}
