import { describe, expect, it } from 'vitest';
import { linksIn } from './links';

describe('linksIn', () => {
  it('keeps absolute http(s) links in order', () => {
    expect(linksIn('https://a.example.test/one http://b.example.test/two')).toEqual([
      'https://a.example.test/one',
      'http://b.example.test/two',
    ]);
  });

  it('finds a link surrounded by prose', () => {
    expect(linksIn('look at https://a.example.test/x today')).toEqual([
      'https://a.example.test/x',
    ]);
  });

  it('splits on newlines as well as spaces', () => {
    expect(linksIn('https://a.example.test/1\n\nhttps://a.example.test/2\n')).toEqual([
      'https://a.example.test/1',
      'https://a.example.test/2',
    ]);
  });

  it('ignores text with no link in it', () => {
    expect(linksIn('nothing to download here')).toEqual([]);
    expect(linksIn('')).toEqual([]);
    expect(linksIn('   \n  ')).toEqual([]);
  });

  it('leaves scheme-less hosts alone rather than guessing', () => {
    expect(linksIn('example.test/file.zip')).toEqual([]);
  });

  it('does not mistake a bare scheme for a link', () => {
    expect(linksIn('https://')).toEqual([]);
  });
});
