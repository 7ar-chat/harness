// Browser error reporting (Sentry protocol: Rustrak, Sentry, GlitchTip) and usage statistics (Umami).
//
// Both are optional and configured by the server at runtime (GET /api/client-config), so one build serves every
// install and an install that sets nothing loads no extra script and makes no extra request. The server's
// Content-Security-Policy allows exactly the origins it names.
//
// Privacy: errors carry the stack, the route and the release; no request bodies, no cookies, no headers (the SDK's
// default PII is off) and no user identity. Usage statistics identify a person by their login only, which Umami
// stores in the operator's own database; the login never goes to the error tracker.
import * as Sentry from "@sentry/browser";

export interface ClientConfig {
  sentry?: { dsn: string; environment?: string; release?: string };
  umami?: { scriptUrl: string; websiteId: string };
}

interface UmamiApi {
  identify: (id: string, data?: Record<string, unknown>) => void;
  track: (...a: unknown[]) => void;
}
declare global {
  interface Window {
    umami?: UmamiApi;
  }
}

let errorsOn = false;
let identified = "";

/** Starts error reporting when the config names a DSN. Safe to call twice. */
export function startErrorReporting(cfg: ClientConfig["sentry"]): boolean {
  if (!cfg?.dsn || errorsOn) return errorsOn;
  Sentry.init({
    dsn: cfg.dsn,
    environment: cfg.environment,
    release: cfg.release,
    // Collect nothing the user typed or the browser stored: the stack, the route and the release are what a bug
    // report needs. (The v11 defaults collect cookies, headers and query parameters.)
    dataCollection: { userInfo: false, cookies: false, httpHeaders: false, httpBodies: [], urlQueryParams: false, stackFrameVariables: false },
    // The app is a single page behind a hash router: the hash is the route. Never the query string, which can
    // carry a login `next` target.
    beforeSend: (event) => {
      if (event.request?.url) event.request.url = event.request.url.replace(/\?[^#]*/, "");
      return event;
    },
    ignoreErrors: [
      // Browser noise that is not ours.
      "ResizeObserver loop limit exceeded",
      "ResizeObserver loop completed with undelivered notifications.",
      // A closed tab or a dropped network aborts fetches and streams.
      "AbortError",
      "Load failed",
      "Failed to fetch",
      "NetworkError when attempting to fetch resource.",
    ],
    // Extensions inject scripts whose errors are not about this app.
    denyUrls: [/^chrome-extension:\/\//, /^moz-extension:\/\//, /^safari-extension:\/\//],
  });
  errorsOn = true;
  return true;
}

/** Reports a caught error (the React error boundary and the router's error component). */
export function reportError(error: unknown, context?: Record<string, unknown>) {
  if (!errorsOn) return;
  Sentry.captureException(error, context ? { extra: context } : undefined);
}

/** Loads the Umami tracker. Umami counts the page views itself, including the hash router's navigations. */
export function startAnalytics(cfg: ClientConfig["umami"], doc: Document = document): boolean {
  if (!cfg?.scriptUrl || !cfg.websiteId) return false;
  if (doc.querySelector("script[data-website-id]")) return true;
  const s = doc.createElement("script");
  s.defer = true;
  s.src = cfg.scriptUrl;
  s.dataset.websiteId = cfg.websiteId;
  // The route lives in the hash (/#/p/<project>), so the hash is part of the page: keep it, drop the query string.
  s.dataset.excludeSearch = "true";
  s.dataset.doNotTrack = "true";
  doc.head.append(s);
  return true;
}

/**
 * Tells Umami who the signed-in person is: their login, nothing else. The tracker script loads asynchronously, so
 * this polls for it for a few seconds and gives up quietly (statistics are optional). Repeating the same login is
 * free.
 */
export function identifyUser(login: string | undefined, win: Window = window) {
  if (!login || login === identified) return;
  let tries = 0;
  const attempt = () => {
    if (win.umami) {
      win.umami.identify(login);
      identified = login;
    } else if (++tries <= 40) {
      setTimeout(attempt, 150);
    }
  };
  attempt();
}

/** Fetches the server's client config. A server without it (older build, plain API) means "nothing to start". */
export async function loadClientConfig(fetchImpl: typeof fetch = fetch): Promise<ClientConfig> {
  try {
    const r = await fetchImpl("/api/client-config", { credentials: "same-origin" });
    return r.ok ? ((await r.json()) as ClientConfig) : {};
  } catch {
    return {};
  }
}

/** For tests. */
export function resetTelemetryForTests() {
  errorsOn = false;
  identified = "";
}
