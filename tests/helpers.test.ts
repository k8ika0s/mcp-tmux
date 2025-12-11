import { describe, expect, it } from 'vitest';
import { buildPath } from '../src/index.js';

describe('buildPath', () => {
  it('appends fallbacks to an existing PATH', () => {
    const result = buildPath('foo:/bin', ['a', 'b']);
    expect(result).toBe('foo:/bin:a:b');
  });

  it('handles undefined PATH', () => {
    const result = buildPath(undefined, ['a']);
    expect(result).toBe('a');
  });

  it('skips duplicates', () => {
    const result = buildPath('a:b', ['b', 'c']);
    expect(result).toBe('a:b:c');
  });
});
