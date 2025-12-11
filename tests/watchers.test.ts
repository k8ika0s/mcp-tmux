import { describe, expect, it } from 'vitest';
import { diffNewFiles, buildPath } from '../src/index.js';

describe('diffNewFiles', () => {
  it('detects added files', () => {
    expect(diffNewFiles(['a', 'b'], ['a', 'b', 'c'])).toEqual(['c']);
  });

  it('no additions', () => {
    expect(diffNewFiles(['a'], ['a'])).toEqual([]);
  });
});

describe('buildPath', () => {
  it('deduplicates additions', () => {
    const result = buildPath('x:y', ['y', 'z']);
    expect(result).toBe('x:y:z');
  });
});
