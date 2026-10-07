import { apiFetch } from './client'

export interface LoginResponse {
  token: string
}

export interface MeResponse {
  user: string
  // The masked admin access key. The server fills it for the admin session only
  // and sends "" for everyone else.
  accessKey: string
}

// isAdminSession mirrors the server's rule: only the "admin" subject may use the
// admin routes (keys, IAM, settings and the rest), every other session gets 403.
export function isAdminSession(me: MeResponse | null | undefined): boolean {
  return me?.user === 'admin'
}

// sessionLabel is what the top bar shows for the signed-in session. A non-admin
// session has no access key in the answer, so it falls back to the user name
// rather than showing nothing.
export function sessionLabel(me: MeResponse | null | undefined): string {
  return me?.accessKey || me?.user || ''
}

export function login(accessKey: string, secretKey: string): Promise<LoginResponse> {
  return apiFetch<LoginResponse>('/auth/login', {
    method: 'POST',
    body: JSON.stringify({ accessKey, secretKey }),
  })
}

export function getMe(): Promise<MeResponse> {
  return apiFetch<MeResponse>('/auth/me')
}

export interface OIDCConfigResponse {
  enabled: boolean
  issuerUrl?: string
  clientId?: string
  /** Authorization endpoint discovered from the provider. Not derivable from the
   *  issuer: many providers serve it at an unrelated path (issue #44). */
  authorizeUrl?: string
  /** Which OAuth flow to drive. "code" is the modern authorization-code flow with
   *  PKCE and the only one Authentik and Keycloak enable by default. */
  flow?: 'code' | 'implicit'
  /** Scopes the provider actually accepts, negotiated from its discovery
   *  document. Asking for one it does not define fails the login outright. */
  scope?: string
}

export interface OIDCLoginResponse {
  token: string
  user: string
  email: string
}

export function getOIDCConfig(): Promise<OIDCConfigResponse> {
  return apiFetch<OIDCConfigResponse>('/auth/oidc/config')
}

/**
 * Builds the URL that starts an SSO login.
 *
 * The base must come from the provider's discovery document. Appending
 * "/authorize" to the issuer breaks on every provider that serves a global
 * authorization endpoint while giving each application its own issuer path
 * (Authentik, Keycloak, Auth0) — the URL simply does not exist there (issue #44).
 * The issuer-derived guess remains only as a fallback for providers that publish
 * no authorization_endpoint at all.
 */
export function buildOIDCAuthorizeUrl(
  config: OIDCConfigResponse,
  redirectUri: string,
  nonce: string,
): string {
  const base = config.authorizeUrl || `${config.issuerUrl}/authorize`
  // A discovered endpoint may already carry query parameters of its own.
  const sep = base.includes('?') ? '&' : '?'
  return (
    `${base}${sep}response_type=id_token` +
    `&client_id=${encodeURIComponent(config.clientId ?? '')}` +
    `&redirect_uri=${encodeURIComponent(redirectUri)}` +
    `&scope=${encodeURIComponent(config.scope || 'openid email profile')}` +
    `&nonce=${encodeURIComponent(nonce)}` +
    `&response_mode=fragment`
  )
}

/** Asks the server to begin an authorization-code login. The PKCE verifier and
 *  nonce stay on the server, sealed into the state carried by this URL. */
export function startOIDCLogin(redirectUri: string): Promise<{ authorizeUrl: string }> {
  return apiFetch<{ authorizeUrl: string }>('/auth/oidc/start', {
    method: 'POST',
    body: JSON.stringify({ redirectUri }),
  })
}

/** Hands the authorization code back for the server to redeem. */
export function completeOIDCLogin(code: string, state: string): Promise<OIDCLoginResponse> {
  return apiFetch<OIDCLoginResponse>('/auth/oidc/callback', {
    method: 'POST',
    body: JSON.stringify({ code, state }),
  })
}

export function oidcLogin(idToken: string): Promise<OIDCLoginResponse> {
  return apiFetch<OIDCLoginResponse>('/auth/oidc', {
    method: 'POST',
    body: JSON.stringify({ idToken }),
  })
}
