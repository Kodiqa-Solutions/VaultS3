import { describe, it, expect } from 'vitest'
import { encodeSegment, encodeKeyPath, isAddressableKey, UnsafePathError } from './paths'

// The server routes on the decoded path. Whatever these helpers produce must
// decode back to exactly the value that went in, and nothing in it may be read
// as a query, a fragment or an extra path segment.
function decodesTo(encoded: string): string {
  return encoded.split('/').map(decodeURIComponent).join('/')
}

describe('encodeSegment', () => {
  const cases: [string, string][] = [
    ['a?b', 'a%3Fb'],
    ['a#b', 'a%23b'],
    ['100%', '100%25'],
    ['a%2Fb', 'a%252Fb'],
    ['two words', 'two%20words'],
    ['a+b', 'a%2Bb'],
    ['naïve-用户', 'na%C3%AFve-%E7%94%A8%E6%88%B7'],
    ['AKIAEXAMPLE', 'AKIAEXAMPLE'],
  ]
  for (const [input, want] of cases) {
    it(`encodes ${JSON.stringify(input)}`, () => {
      expect(encodeSegment(input)).toBe(want)
      expect(decodeURIComponent(encodeSegment(input))).toBe(input)
    })
  }

  it('leaves no character that ends the path or splits it', () => {
    for (const v of ['a?b', 'a#b', 'x%3Fy', 'a&b=c', 'a;b']) {
      expect(encodeSegment(v)).not.toMatch(/[?#/&;]/)
    }
  })

  // %2F is decoded into a real '/' before the server routes, so a name holding
  // one would reach a different handler, for example a group membership.
  it('refuses a value it cannot carry as one segment', () => {
    for (const v of ['', 'x/groups/admins', '.', '..']) {
      expect(() => encodeSegment(v)).toThrow(UnsafePathError)
    }
  })
})

describe('encodeKeyPath', () => {
  const cases: [string, string][] = [
    ['photo.jpg', 'photo.jpg'],
    ['dir/sub/file.txt', 'dir/sub/file.txt'],
    ['dir/what?.txt', 'dir/what%3F.txt'],
    ['dir/#1.txt', 'dir/%231.txt'],
    ['50%/off', '50%25/off'],
    ['a%2Fb', 'a%252Fb'],
    ['my docs/q 1.pdf', 'my%20docs/q%201.pdf'],
    ['c++/notes', 'c%2B%2B/notes'],
    ['日本/ファイル.txt', '%E6%97%A5%E6%9C%AC/%E3%83%95%E3%82%A1%E3%82%A4%E3%83%AB.txt'],
  ]
  for (const [input, want] of cases) {
    it(`encodes ${JSON.stringify(input)} segment by segment`, () => {
      expect(encodeKeyPath(input)).toBe(want)
      expect(decodesTo(encodeKeyPath(input))).toBe(input)
    })
  }

  it('keeps the slashes of a nested key as separators', () => {
    expect(encodeKeyPath('a/b/c').split('/')).toHaveLength(3)
  })

  // The browser collapses dot segments (even as %2e) and the server's mux cleans
  // '//' and the API trims a trailing '/', so each of these would act on a
  // different key than the one asked for.
  it('refuses a key the URL would rewrite into another key', () => {
    for (const k of ['', 'a/../b', '../b', 'a/./b', 'a//b', '/a', 'dir/']) {
      expect(isAddressableKey(k)).toBe(false)
      expect(() => encodeKeyPath(k)).toThrow(UnsafePathError)
    }
  })

  it('accepts dots that are not a whole segment', () => {
    for (const k of ['.env', 'a/..b', 'a.../b', 'v1.2/x']) {
      expect(isAddressableKey(k)).toBe(true)
    }
  })
})
