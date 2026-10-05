import { describe, expect, it } from 'vitest';
import { ApiError } from './api';
import {
  MAX_INPUT_FRAME, chunkUTF8, closeReasonMessage, connectTerminal, terminalSocketURL,
  ticketErrorMessage,
} from './terminal';
import type { SocketLike, TerminalDeps, TerminalState } from './terminal';

class FakeSocket implements SocketLike {
  static all: FakeSocket[] = [];
  readyState = 0;
  binaryType = 'blob';
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  sent: (string | Uint8Array<ArrayBuffer>)[] = [];
  closed = false;
  url: string;
  constructor(url: string) { this.url = url; FakeSocket.all.push(this); }
  send(data: string | Uint8Array<ArrayBuffer>) { this.sent.push(data); }
  close() { this.closed = true; this.readyState = 3; }
  open() { this.readyState = 1; this.onopen?.(new Event('open')); }
  message(data: string | ArrayBuffer) { this.onmessage?.(new MessageEvent('message', { data })); }
  serverClose(code: number, reason: string) {
    this.readyState = 3;
    this.onclose?.({ code, reason } as CloseEvent);
  }
}

const tick = () => new Promise((r) => setTimeout(r, 0));

function setup(over: Partial<TerminalDeps> = {}) {
  FakeSocket.all = [];
  const states: TerminalState[] = [];
  const output: Uint8Array[] = [];
  let tickets = 0;
  const handle = connectTerminal({
    requestTicket: async () => ({ ticket: `tk-${++tickets}` }),
    createSocket: (url) => new FakeSocket(url),
    url: 'wss://kb.example:8443/ws/terminal',
    onState: (s) => states.push(s),
    onOutput: (b) => output.push(b),
    ...over,
  });
  return { handle, states, output, tickets: () => tickets, last: () => states[states.length - 1] };
}

describe('terminalSocketURL', () => {
  it('uses wss on https and carries no query', () => {
    expect(terminalSocketURL({ protocol: 'https:', host: 'kb:8443' })).toBe('wss://kb:8443/ws/terminal');
    expect(terminalSocketURL({ protocol: 'http:', host: 'localhost:5173' })).toBe('ws://localhost:5173/ws/terminal');
  });
});

describe('connectTerminal', () => {
  it('sends the ticket as the first text frame, never in the URL', async () => {
    const t = setup();
    await tick();
    const ws = FakeSocket.all[0];
    expect(ws.url).toBe('wss://kb.example:8443/ws/terminal');
    expect(ws.url).not.toContain('tk-1');
    expect(ws.binaryType).toBe('arraybuffer');
    t.handle.input('ls\r'); // not open yet: dropped so the ticket stays first
    expect(ws.sent).toEqual([]);
    ws.open();
    expect(ws.sent[0]).toBe(JSON.stringify({ ticket: 'tk-1' }));
    expect(t.last().phase).toBe('waiting');
  });

  it('goes live on the first relayed frame and forwards input and resize', async () => {
    const t = setup();
    await tick();
    const ws = FakeSocket.all[0];
    ws.open();
    ws.message(new Uint8Array([36, 32]).buffer);
    expect(t.last().phase).toBe('live');
    expect(Array.from(t.output[0])).toEqual([36, 32]);
    t.handle.input('x');
    t.handle.resize(120, 40);
    expect(ws.sent[1]).toEqual(new TextEncoder().encode('x'));
    expect(ws.sent[2]).toBe(JSON.stringify({ type: 'resize', cols: 120, rows: 40 }));
  });

  it('reports the exit code, treating an omitted code as 0', async () => {
    const t = setup();
    await tick();
    const ws = FakeSocket.all[0];
    ws.open();
    ws.message('{"type":"exit"}');
    ws.serverClose(1000, 'session ended');
    expect(t.last()).toMatchObject({ phase: 'ended', exitCode: 0, message: 'Process exited with code 0.' });

    const t2 = setup();
    await tick();
    FakeSocket.all[0].open();
    FakeSocket.all[0].message('{"type":"exit","code":130}');
    FakeSocket.all[0].serverClose(1000, 'session ended');
    expect(t2.last()).toMatchObject({ exitCode: 130, tone: 'error' });
  });

  it('surfaces server close reasons', async () => {
    const t = setup();
    await tick();
    FakeSocket.all[0].open();
    FakeSocket.all[0].serverClose(1013, 'pairing timeout');
    expect(t.last()).toMatchObject({ phase: 'ended', tone: 'error' });
    expect(t.last().message).toMatch(/pairing timeout/);
  });

  it('surfaces ticket POST errors without opening a socket', async () => {
    const t = setup({ requestTicket: async () => { throw new ApiError(409, 'session_limit'); } });
    await tick();
    expect(FakeSocket.all).toHaveLength(0);
    expect(t.last()).toMatchObject({ phase: 'ended', tone: 'error' });
    expect(t.last().message).toMatch(/maximum of 2/);
  });

  it('closes the socket on unmount and emits nothing after', async () => {
    const t = setup();
    await tick();
    const ws = FakeSocket.all[0];
    ws.open();
    t.handle.close();
    expect(ws.closed).toBe(true);
    const n = t.states.length;
    ws.serverClose(1000, 'session ended');
    expect(t.states).toHaveLength(n);
  });

  it('StrictMode mount/unmount/mount requests one ticket and opens one socket', async () => {
    FakeSocket.all = [];
    let tickets = 0;
    const deps: TerminalDeps = {
      requestTicket: async () => ({ ticket: `tk-${++tickets}` }),
      createSocket: (url) => new FakeSocket(url),
      url: 'wss://kb/ws/terminal',
      onState: () => {},
      onOutput: () => {},
    };
    connectTerminal(deps).close();
    connectTerminal(deps);
    await tick();
    expect(tickets).toBe(1);
    expect(FakeSocket.all).toHaveLength(1);
    expect(FakeSocket.all[0].closed).toBe(false);
  });

  it('does not open a socket when cancelled after the ticket was issued', async () => {
    let release!: (v: { ticket: string }) => void;
    const t = setup({ requestTicket: () => new Promise((r) => { release = r; }) });
    await tick();
    t.handle.close();
    release({ ticket: 'late' });
    await tick();
    expect(FakeSocket.all).toHaveLength(0);
  });

  it('closes a still-connecting socket without sending the ticket', async () => {
    const t = setup();
    await tick();
    const ws = FakeSocket.all[0];
    t.handle.close();
    expect(ws.closed).toBe(true);
    expect(ws.sent).toEqual([]);
  });
});

describe('chunkUTF8', () => {
  const enc = new TextEncoder();
  const dec = new TextDecoder('utf-8', { fatal: true });
  const join = (parts: Uint8Array[]) => {
    const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
    let o = 0;
    for (const p of parts) { out.set(p, o); o += p.length; }
    return out;
  };

  it('returns nothing for empty input and one frame when it fits', () => {
    expect(chunkUTF8(enc.encode(''))).toEqual([]);
    const small = enc.encode('ls -la\r');
    expect(chunkUTF8(small)).toEqual([small]);
    const exact = enc.encode('a'.repeat(MAX_INPUT_FRAME));
    expect(chunkUTF8(exact)).toHaveLength(1);
  });

  it('splits ASCII into full frames by byte length', () => {
    const parts = chunkUTF8(enc.encode('a'.repeat(MAX_INPUT_FRAME * 2 + 5)));
    expect(parts.map((p) => p.length)).toEqual([MAX_INPUT_FRAME, MAX_INPUT_FRAME, 5]);
  });

  it('measures encoded bytes, not string length', () => {
    // 'é' is 2 bytes: 10000 chars is 20000 bytes, over one 16 KiB frame.
    const s = '\u00e9'.repeat(10000);
    const parts = chunkUTF8(enc.encode(s));
    expect(parts.length).toBe(2);
    for (const p of parts) expect(p.length).toBeLessThanOrEqual(MAX_INPUT_FRAME);
  });

  it('never splits a multi-byte sequence or surrogate pair', () => {
    // Mix 1-, 2-, 3- and 4-byte (surrogate pair) characters at odd offsets.
    const s = 'a\u00e9\u20ac\u{1F600}'.repeat(5000) + 'x\u{1F680}';
    for (const max of [4, 5, 7, 13, 1024, MAX_INPUT_FRAME]) {
      const parts = chunkUTF8(enc.encode(s), max);
      for (const p of parts) {
        expect(p.length).toBeGreaterThan(0);
        expect(p.length).toBeLessThanOrEqual(max);
        expect(() => dec.decode(p)).not.toThrow(); // each frame is valid UTF-8 on its own
      }
      expect(dec.decode(join(parts))).toBe(s);
    }
  });

  it('input() sends a large paste as ordered binary frames under the limit', async () => {
    const t = setup();
    await tick();
    const ws = FakeSocket.all[0];
    ws.open();
    ws.message(new Uint8Array([36]).buffer);
    const paste = '\u{1F600}x'.repeat(20000); // 100000 bytes
    t.handle.input(paste);
    const frames = ws.sent.slice(1);
    expect(frames.length).toBe(Math.ceil(100000 / MAX_INPUT_FRAME));
    for (const f of frames) {
      expect(f).toBeInstanceOf(Uint8Array);
      expect((f as Uint8Array).length).toBeLessThanOrEqual(MAX_INPUT_FRAME);
    }
    expect(dec.decode(join(frames as Uint8Array[]))).toBe(paste);
  });
});

describe('closeReasonMessage', () => {
  it.each([
    [1013, 'pairing timeout', /pairing timeout/, 'error'],
    [1008, 'agent refused', /refused/, 'error'],
    [1008, 'invalid or expired ticket', /invalid or expired/, 'error'],
    [1001, 'ticket expired', /invalid or expired/, 'error'],
    [1001, 'idle timeout', /30 minutes of inactivity/, 'info'],
    [1001, 'cancelled', /cancelled/, 'error'],
    [1008, 'session closed', /before it started/, 'error'],
    [1000, 'session ended', /Session ended/, 'info'],
    [1006, '', /Connection lost/, 'error'],
    [4000, 'weird', /code 4000: weird/, 'error'],
  ] as const)('%i %s', (code, reason, re, tone) => {
    const m = closeReasonMessage(code, reason);
    expect(m.message).toMatch(re);
    expect(m.tone).toBe(tone);
  });
});

describe('ticketErrorMessage', () => {
  it('maps server error codes', () => {
    expect(ticketErrorMessage(new ApiError(403, 'terminal_not_enabled'))).toMatch(/disabled/);
    expect(ticketErrorMessage(new ApiError(409, 'not_connected'))).toMatch(/not connected/);
    expect(ticketErrorMessage(new ApiError(404, 'not found'))).toBe('Device not found.');
    expect(ticketErrorMessage(new Error('boom'))).toMatch(/boom/);
  });
});
