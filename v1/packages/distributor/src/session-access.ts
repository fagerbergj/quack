/** A caller on the session listener: the session its launch credential belongs to, and whether its agent holds the grant to reach other sessions. */
export type SessionCaller = { key: string; crossSession: boolean };

// What a session may ask of a session through the distributor; deciding, housekeeping and page sockets are for people, so none of them is here.
const SESSION_ROUTES = new Set(['GET ', 'GET /ping', 'GET /messages', 'GET /meta', 'GET /approvals', 'GET /artifacts', 'GET /questions', 'GET /failures', 'GET /export', 'POST /invocations', 'POST /abort', 'POST /resume']);
const LISTINGS = /^\/(sessions|index\/[a-z]+)$/;

const decoded = (s: string) => {
  try {
    return decodeURIComponent(s);
  } catch {
    return undefined;
  }
};

/** Why the session listener refuses a request, or undefined when the caller's credential reaches it. The broker, apps and fact publishing check the credential themselves. */
export function sessionRefusal(method: string, pathname: string, caller: SessionCaller): [number, string] | undefined {
  if (/^\/(proxy|apps)\//.test(pathname) || (method === 'POST' && pathname === '/index/facts')) return undefined;
  const one = /^\/sessions\/([^/]+)(\/[^/]+)?$/.exec(pathname);
  if (one && SESSION_ROUTES.has(`${method} ${one[2] ?? ''}`)) return caller.crossSession || decoded(one[1]!) === caller.key ? undefined : OWN_KEY_ONLY;
  if (method === 'GET' && LISTINGS.test(pathname)) return caller.crossSession ? undefined : OWN_KEY_ONLY;
  return [403, 'sessions may not call this route'];
}

const OWN_KEY_ONLY: [number, string] = [403, 'this session may reach only its own key'];
