import { makeTranslator } from '../i18n'

// Every name or object key that goes into an API path must pass through one of
// these two helpers. The server routes on the DECODED path, so an unencoded '?'
// ends the path, '#' starts a fragment and '%41' arrives as 'A'. Deleting the
// user "a?b" used to send DELETE /iam/users/a?b and delete the user "a"
// (issue #62 in the CLI, the same bug class here).

// API functions that call these are declared async, so a refusal reaches the
// caller as a rejected promise, which every page already handles, and never as
// a synchronous throw escaping a .then() chain.

// UnsafePathError is thrown for a value that no encoding can carry to the
// server intact. Refusing is the only safe answer: sending it would act on a
// different user, group or object than the one on screen.
export class UnsafePathError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'UnsafePathError'
  }
}

// The message is read in the language the page is showing. The API layer has
// no React context, but the i18n provider keeps <html lang> in step with the
// chosen locale, so that is the one place both can see.
function translate(key: string, vars: Record<string, string>): string {
  const lang = typeof document !== 'undefined' ? document.documentElement.lang : ''
  return makeTranslator(lang || 'en')(key, vars)
}

// A '.' or '..' segment is removed or collapsed by the browser's URL parser
// before the request is sent, and the percent encoded forms (%2e) are treated
// the same way, so "photos/../secret" would be sent as "secret".
function isDotSegment(segment: string): boolean {
  return segment === '.' || segment === '..'
}

// An empty segment comes from a leading or trailing '/' or from '//'. The
// server's mux redirects "a//b" to the cleaned "a/b" and the API strips a
// trailing '/', so "dir/" would act on the object "dir".
function isUnsafeKeySegment(segment: string): boolean {
  return segment === '' || isDotSegment(segment)
}

// encodeSegment encodes a value that fills exactly one path segment: a user,
// group, policy, bucket, access key or snapshot id. A '/' cannot be carried
// because the server decodes %2F back into a separator before it routes, so
// "x/groups/admins" would reach a different handler. The CLI refuses it too.
export function encodeSegment(value: string): string {
  if (value === '' || value.includes('/') || isDotSegment(value)) {
    throw new UnsafePathError(translate('errors.unsafeName', { name: value }))
  }
  return encodeURIComponent(value)
}

// isAddressableKey reports whether an object key can be sent in a URL path
// without the browser or the server rewriting it into a different key.
export function isAddressableKey(key: string): boolean {
  return !key.split('/').some(isUnsafeKeySegment)
}

// encodeKeyPath encodes an object key for the path. Its '/' characters are real
// separators that the server joins back into the key, so each segment is
// encoded on its own and the slashes are kept.
export function encodeKeyPath(key: string): string {
  if (!isAddressableKey(key)) {
    throw new UnsafePathError(translate('errors.unsafeKey', { key }))
  }
  return key.split('/').map(encodeURIComponent).join('/')
}
