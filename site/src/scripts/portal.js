// Optional telemetry of the home page: error reporting (Sentry protocol: Rustrak, Sentry, GlitchTip) and Umami.
//
// The container writes /client-config.json at start from SENTRY_BROWSER_DSN, UMAMI_URL and UMAMI_WEBSITE_ID, and a
// Content-Security-Policy that allows exactly those origins. With none set the file is "{}", this script does
// nothing, and the error-reporting code below is never downloaded.
//
// The privacy settings mirror the app's (web/src/lib/telemetry.ts): no cookies, headers, bodies, query strings or
// user identity are collected. The home page has no login, so Umami sees anonymous visits only.
async function main() {
  let cfg = {};
  try {
    const r = await fetch("/client-config.json", { credentials: "omit" });
    if (r.ok) cfg = await r.json();
  } catch {
    return;
  }

  if (cfg.umami?.scriptUrl && cfg.umami.websiteId && !document.querySelector("script[data-website-id]")) {
    const s = document.createElement("script");
    s.defer = true;
    s.src = cfg.umami.scriptUrl;
    s.dataset.websiteId = cfg.umami.websiteId;
    s.dataset.excludeSearch = "true";
    s.dataset.doNotTrack = "true";
    document.head.append(s);
  }

  if (cfg.sentry?.dsn) {
    const Sentry = await import("@sentry/browser");
    Sentry.init({
      dsn: cfg.sentry.dsn,
      environment: cfg.sentry.environment,
      release: cfg.sentry.release,
      dataCollection: { userInfo: false, cookies: false, httpHeaders: false, httpBodies: [], urlQueryParams: false, stackFrameVariables: false },
      beforeSend: (event) => {
        if (event.request?.url) event.request.url = event.request.url.replace(/\?[^#]*/, "");
        return event;
      },
      ignoreErrors: ["ResizeObserver loop limit exceeded", "ResizeObserver loop completed with undelivered notifications.", "AbortError", "Load failed", "Failed to fetch", "NetworkError when attempting to fetch resource."],
      denyUrls: [/^chrome-extension:\/\//, /^moz-extension:\/\//, /^safari-extension:\/\//],
    });
  }
}

main();
