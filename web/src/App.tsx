import { useEffect, useState } from 'react';
import { BrowserRouter, Routes, Route, Link, useNavigate } from 'react-router';
import { api, wakePath } from './api';
import { setAuthNavigator } from './auth';
import { useEvents } from './useEvents';
import { Devices } from './views/Devices';
import { DeviceDetail } from './views/DeviceDetail';
import { Login } from './views/Login';
import { Register } from './views/Register';
import { Audit } from './views/Audit';
import { TerminalView } from './views/Terminal';
import { ThemeToggle } from './components/ThemeToggle';

function Shell() {
  const navigate = useNavigate();
  const [flash, setFlash] = useState('');
  const { state, live } = useEvents();

  async function handleLogout() {
    try {
      await api('/api/logout', { method: 'POST' });
    } catch { /* ignore */ }
    navigate('/login');
  }

  async function wakeByName(name: string) {
    try {
      await api(wakePath(name), { method: 'POST' });
      setFlash(`Wake-on-LAN sent to ${name}`);
    } catch (err) {
      setFlash(`Wake failed: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  return (
    <div className="min-h-screen bg-canvas text-fg">
      <nav className="bg-surface border-b border-border px-4 py-3">
        <div className="max-w-6xl mx-auto flex items-center justify-between">
          <div className="flex items-center gap-6">
            <Link to="/" className="text-lg font-bold text-fg">KumaBoard</Link>
            <Link to="/" className="text-sm text-fg-muted hover:text-fg">Devices</Link>
            <Link to="/register" className="text-sm text-fg-muted hover:text-fg">Register</Link>
            <Link to="/audit" className="text-sm text-fg-muted hover:text-fg">Audit</Link>
          </div>
          <div className="flex items-center gap-3">
            <ThemeToggle />
            <button
              onClick={handleLogout}
              className="text-sm text-fg-subtle hover:text-fg"
            >
              Logout
            </button>
          </div>
        </div>
      </nav>

      {flash && (
        <div className="max-w-6xl mx-auto mt-4 px-4">
          <div className="rounded bg-info-soft p-3 text-sm text-info flex justify-between">
            <span>{flash}</span>
            <button onClick={() => setFlash('')} className="ml-4 text-info hover:text-fg">x</button>
          </div>
        </div>
      )}

      <main className="max-w-6xl mx-auto p-4">
        <Routes>
          <Route path="/" element={<Devices state={state} live={live} onWake={wakeByName} />} />
          <Route path="/devices/:name" element={<DeviceDetail state={state} live={live} onWake={wakeByName} />} />
          <Route path="/devices/:name/terminal" element={<TerminalView />} />
          <Route path="/register" element={<Register />} />
          <Route path="/audit" element={<Audit />} />
        </Routes>
      </main>
    </div>
  );
}

// AuthRedirect wires api()'s central 401 handling to the router.
function AuthRedirect() {
  const navigate = useNavigate();
  useEffect(() => setAuthNavigator({
    navigate: (to) => navigate(to, { replace: true }),
    currentPath: () => window.location.pathname + window.location.search,
  }), [navigate]);
  return null;
}

export default function App() {
  return (
    <BrowserRouter>
      <AuthRedirect />
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="/*" element={<Shell />} />
      </Routes>
    </BrowserRouter>
  );
}
