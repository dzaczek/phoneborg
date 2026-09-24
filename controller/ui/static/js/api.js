// Admin API client. The token lives in sessionStorage (this tab only) and is
// sent as a bearer header; it never goes into URLs or logs.

const TOKEN_KEY = 'phoneborg.adminToken';
const TIMEOUT_MS = 10000;

export const token = {
  get() { try { return sessionStorage.getItem(TOKEN_KEY) || ''; } catch { return ''; } },
  set(t) { try { sessionStorage.setItem(TOKEN_KEY, t); } catch { /* private mode: keep nothing */ } },
  clear() { try { sessionStorage.removeItem(TOKEN_KEY); } catch { /* ignore */ } },
};

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status; // 0 = network error or timeout
  }
}

// Called on 401 (token rejected) and 503 (admin API disabled).
let onAuthError = () => {};
export const setAuthErrorHandler = (fn) => { onAuthError = fn; };

export async function api(method, path, body, tok = token.get()) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), TIMEOUT_MS);
  const headers = { Authorization: 'Bearer ' + tok, Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res;
  try {
    res = await fetch(path, {
      method, headers, signal: ctl.signal, cache: 'no-store', credentials: 'omit',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (e) {
    throw new ApiError(0, e.name === 'AbortError' ? 'The controller did not answer within 10 s.' : 'Cannot reach the controller.');
  } finally {
    clearTimeout(timer);
  }
  const text = res.status === 204 ? '' : await res.text().catch(() => '');
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* not JSON, e.g. "404 page not found" */ }
  if (!res.ok) {
    const msg = (data && data.error) || text.trim() || res.statusText || 'HTTP ' + res.status;
    const err = new ApiError(res.status, msg);
    if (res.status === 401 || res.status === 503) onAuthError(err);
    throw err;
  }
  return data;
}

export const get = (path) => api('GET', path);

// getOptional returns null when the route does not exist on this controller
// (404), e.g. model management on a controller built without it.
export async function getOptional(path) {
  try {
    return await api('GET', path);
  } catch (e) {
    if (e.status === 404) return null;
    throw e;
  }
}
