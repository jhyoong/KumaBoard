import { type FormEvent, useState } from 'react';
import { api, ApiError } from '../api';

export function Register() {
  const [name, setName] = useState('');
  const [token, setToken] = useState<string | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError('');
    setLoading(true);
    try {
      const res = await api<{ token: string }>('/api/devices', {
        method: 'POST',
        body: JSON.stringify({ name }),
      });
      setToken(res.token);
    } catch (err) {
      if (err instanceof ApiError) {
        setError(err.message);
      } else {
        setError('Registration failed');
      }
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="max-w-md mx-auto">
      <h2 className="text-xl font-semibold text-fg mb-4">Register Device</h2>

      {token ? (
        <div className="rounded-lg border border-success-line bg-success-soft p-6">
          <p className="text-sm text-success mb-2 font-medium">Device registered. Save this token -- it will not be shown again:</p>
          <code className="block bg-inset text-fg rounded p-3 text-sm font-mono break-all border border-success-line">
            {token}
          </code>
        </div>
      ) : (
        <form onSubmit={handleSubmit} className="space-y-4">
          {error && (
            <div className="rounded bg-danger-soft p-3 text-sm text-danger">{error}</div>
          )}
          <label className="block">
            <span className="block text-sm font-medium text-fg-muted mb-1">Device Name</span>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
              autoFocus
              placeholder="e.g. homelab-01"
              className="w-full rounded border border-border-strong bg-inset px-3 py-2 text-sm focus:border-focus focus:outline-none focus:ring-1 focus:ring-focus"
            />
          </label>
          <button
            type="submit"
            disabled={loading}
            className="w-full rounded bg-accent px-4 py-2 text-sm font-medium text-on-accent hover:bg-accent-hover disabled:opacity-50"
          >
            {loading ? 'Registering...' : 'Register'}
          </button>
        </form>
      )}
    </div>
  );
}
