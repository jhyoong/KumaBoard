import { describe, it, expect } from 'vitest';
import { TEMP_CRIT_C, TEMP_WARN_C, tempClass, tempLevel, tempText } from './temp';

describe('tempLevel', () => {
  it('switches at the warn and crit boundaries', () => {
    expect(TEMP_WARN_C).toBe(80);
    expect(TEMP_CRIT_C).toBe(95);
    expect(tempLevel(20)).toBe('ok');
    expect(tempLevel(79.9)).toBe('ok');
    expect(tempLevel(80)).toBe('warn');
    expect(tempLevel(94.9)).toBe('warn');
    expect(tempLevel(95)).toBe('crit');
    expect(tempLevel(110)).toBe('crit');
  });
});

describe('tempText', () => {
  it('renders whole degrees', () => {
    expect(tempText(54)).toBe('54°C');
    expect(tempText(54.4)).toBe('54°C');
    expect(tempText(79.6)).toBe('80°C');
  });
});

describe('tempClass', () => {
  it('colours by level', () => {
    expect(tempClass(79.9)).toBe('');
    expect(tempClass(80)).toBe('text-warning');
    expect(tempClass(94.9)).toBe('text-warning');
    expect(tempClass(95)).toBe('text-danger font-medium');
  });
});
