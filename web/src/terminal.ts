// Terminal session controller: ticket POST -> /ws/terminal -> ticket as the
// first text frame -> relay. Kept free of React and xterm so the socket
// lifecycle and close-reason mapping can be tested with a fake WebSocket.
//
// The server sends no explicit "paired" signal: the broker does not read or
// write the browser socket until the agent attaches, so the first frame from
// the relay (normally the shell prompt) is what proves the session is live.

import { ApiError } from './api';

export type TerminalPhase = 'requesting' | 'connecting' | 'waiting' | 'live' | 'ended';

export interface TerminalState {
  phase: TerminalPhase;
  // Set once phase is 'ended'.
  message?: string;
  tone?: 'info' | 'error';
  exitCode?: number;
}

// SocketLike is the subset of WebSocket the controller uses.
export interface SocketLike {
  readyState: number;
  binaryType: string;
  onopen: ((ev: Event) => void) | null;
  onmessage: ((ev: MessageEvent) => void) | null;
  onclose: ((ev: CloseEvent) => void) | null;
  onerror: ((ev: Event) => void) | null;
  send(data: string | Uint8Array<ArrayBuffer>): void;
  close(code?: number, reason?: string): void;
}

const OPEN = 1;

export interface TerminalDeps {
  requestTicket: () => Promise<{ ticket: string }>;
  createSocket: (url: string) => SocketLike;
  url: string;
  onState: (s: TerminalState) => void;
  onOutput: (bytes: Uint8Array) => void;
  // Defers the ticket POST so an immediate close() (React StrictMode's
  // mount/unmount/mount) never issues a ticket at all. Defaults to setTimeout.
  defer?: (fn: () => void) => () => void;
}

export interface TerminalHandle {
  input(data: string): void;
  resize(cols: number, rows: number): void;
  // Idempotent. Emits a final 'Disconnected' state unless already ended;
  // nothing is emitted after it.
  close(): void;
}

// terminalSocketURL builds the terminal WebSocket URL. The ticket never goes
// in the URL; it is sent as the first text frame.
export function terminalSocketURL(loc: { protocol: string; host: string }): string {
  const proto = loc.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${loc.host}/ws/terminal`;
}

// MAX_INPUT_FRAME caps each outgoing input frame. The server and agent accept
// frames up to 64 KiB (proto.TerminalMaxFrame); 16 KiB leaves ample headroom.
export const MAX_INPUT_FRAME = 16 * 1024;

// chunkUTF8 splits UTF-8 bytes into views of at most max bytes each, cutting
// only before a lead byte so no multi-byte sequence (and so no surrogate pair,
// which TextEncoder emits as one 4-byte sequence) is ever split.
export function chunkUTF8(
  bytes: Uint8Array<ArrayBuffer>, max = MAX_INPUT_FRAME,
): Uint8Array<ArrayBuffer>[] {
  if (max < 4) throw new RangeError('chunkUTF8: max must be at least 4');
  const out: Uint8Array<ArrayBuffer>[] = [];
  let start = 0;
  while (bytes.length - start > max) {
    let end = start + max;
    // Back up over continuation bytes (10xxxxxx) to a sequence boundary.
    while (end > start && (bytes[end] & 0xc0) === 0x80) end--;
    out.push(bytes.subarray(start, end));
    start = end;
  }
  if (bytes.length > start) out.push(bytes.subarray(start));
  return out;
}

// ticketErrorMessage maps a failed ticket POST to a user-facing message.
export function ticketErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.message) {
      case 'session_limit':
        return 'This device already has the maximum of 2 terminal sessions open. Close one and try again.';
      case 'terminal_not_enabled':
        return 'Terminal is disabled for this device. Enable it in the device settings.';
      case 'terminal_not_capable':
        return 'This device\'s agent does not advertise the terminal capability.';
      case 'not_connected':
        return 'The device is not connected.';
      case 'agent_unreachable':
        return 'Could not reach the device\'s agent.';
    }
    if (err.status === 404) return 'Device not found.';
    return `Could not open terminal: ${err.message}`;
  }
  return `Could not open terminal: ${(err as Error)?.message ?? String(err)}`;
}

// closeReasonMessage maps a terminal socket close to a user-facing message.
// Reasons come from server/terminal/broker.go; the code is the fallback.
export function closeReasonMessage(
  code: number, reason: string, exitCode?: number,
): { message: string; tone: 'info' | 'error' } {
  if (exitCode !== undefined) {
    return { message: `Process exited with code ${exitCode}.`, tone: exitCode === 0 ? 'info' : 'error' };
  }
  switch (reason) {
    case 'idle timeout':
      return { message: 'Session closed after 30 minutes of inactivity.', tone: 'info' };
    case 'pairing timeout':
      return { message: 'The agent did not connect within 30 seconds (pairing timeout).', tone: 'error' };
    case 'agent refused':
      return { message: 'The agent refused to open a terminal. Check that "terminal" is in the device\'s agent capabilities.', tone: 'error' };
    case 'invalid or expired ticket':
    case 'ticket expired':
      return { message: 'The terminal ticket was invalid or expired.', tone: 'error' };
    case 'cancelled':
      return { message: 'The server cancelled the terminal request.', tone: 'error' };
    case 'session closed':
      return { message: 'The session was closed before it started.', tone: 'error' };
    case 'session ended':
    case 'shell exited':
      return { message: 'Session ended.', tone: 'info' };
  }
  switch (code) {
    case 1000:
      return { message: 'Session ended.', tone: 'info' };
    case 1006:
      return { message: 'Connection lost.', tone: 'error' };
    case 1013:
      return { message: 'The agent did not connect in time.', tone: 'error' };
  }
  return { message: `Connection closed (code ${code}${reason ? `: ${reason}` : ''}).`, tone: 'error' };
}

// connectTerminal runs one terminal session attempt. Call close() on unmount.
export function connectTerminal(deps: TerminalDeps): TerminalHandle {
  const defer = deps.defer ?? ((fn) => {
    const t = setTimeout(fn, 0);
    return () => clearTimeout(t);
  });
  const encoder = new TextEncoder();
  let done = false;
  let ws: SocketLike | null = null;
  let phase: TerminalPhase = 'requesting';
  let exitCode: number | undefined;

  const set = (s: TerminalState) => {
    if (done) return;
    phase = s.phase;
    if (s.phase === 'ended') done = true;
    deps.onState(s);
  };

  const live = () => {
    if (phase === 'waiting') set({ phase: 'live' });
  };

  const start = () => {
    deps.requestTicket().then(({ ticket }) => {
      // Cancelled after the ticket was issued: do not open the socket. The
      // unused ticket expires server-side after its 30s TTL.
      if (done) return;
      const sock = deps.createSocket(deps.url);
      ws = sock;
      sock.binaryType = 'arraybuffer';
      set({ phase: 'connecting' });

      sock.onopen = () => {
        if (done) return;
        sock.send(JSON.stringify({ ticket }));
        set({ phase: 'waiting' });
      };
      sock.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          live();
          deps.onOutput(new Uint8Array(ev.data));
          return;
        }
        try {
          const ctrl = JSON.parse(ev.data as string);
          live();
          // code is omitempty on the wire, so exit 0 arrives as {"type":"exit"}.
          if (ctrl.type === 'exit') exitCode = typeof ctrl.code === 'number' ? ctrl.code : 0;
        } catch { /* ignore non-JSON text */ }
      };
      // An error is always followed by close, which carries the detail.
      sock.onerror = () => {};
      sock.onclose = (ev) => {
        ws = null;
        set({ phase: 'ended', exitCode, ...closeReasonMessage(ev.code, ev.reason, exitCode) });
      };
    }).catch((err) => {
      set({ phase: 'ended', message: ticketErrorMessage(err), tone: 'error' });
    });
  };

  // Emitted synchronously so a reused view never shows a stale ended state.
  set({ phase: 'requesting' });
  const cancelStart = defer(start);

  return {
    input(data) {
      if (ws?.readyState === OPEN && phase !== 'connecting') {
        // A large paste is split so no frame exceeds the relay's read limit.
        for (const chunk of chunkUTF8(encoder.encode(data))) ws.send(chunk);
      }
    },
    resize(cols, rows) {
      if (ws?.readyState === OPEN && phase !== 'connecting') {
        ws.send(JSON.stringify({ type: 'resize', cols, rows }));
      }
    },
    close() {
      cancelStart();
      set({ phase: 'ended', message: 'Disconnected.', tone: 'info' });
      done = true;
      const sock = ws;
      ws = null;
      if (sock) {
        sock.onopen = sock.onmessage = sock.onerror = null;
        sock.onclose = null;
        sock.close(1000, 'closed by user');
      }
    },
  };
}
