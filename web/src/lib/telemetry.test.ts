import * as Sentry from "@sentry/browser";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { identifyUser, loadClientConfig, reportError, resetTelemetryForTests, startAnalytics, startErrorReporting } from "./telemetry";

vi.mock("@sentry/browser", () => ({ init: vi.fn(), captureException: vi.fn() }));

beforeEach(() => {
  resetTelemetryForTests();
  document.head.innerHTML = "";
  delete window.umami;
});
afterEach(() => vi.useRealTimers());

describe("loadClientConfig", () => {
  it("returns the server's answer", async () => {
    const cfg = { sentry: { dsn: "https://k@h/1" }, umami: { scriptUrl: "https://u/script.js", websiteId: "w" } };
    const f = vi.fn().mockResolvedValue({ ok: true, json: async () => cfg });
    expect(await loadClientConfig(f as unknown as typeof fetch)).toEqual(cfg);
    expect(f).toHaveBeenCalledWith("/api/client-config", { credentials: "same-origin" });
  });

  it.each([
    ["an older server without the endpoint", () => Promise.resolve({ ok: false, json: async () => ({}) })],
    ["a network failure", () => Promise.reject(new Error("offline"))],
    ["a body that is not JSON", () => Promise.resolve({ ok: true, json: async () => Promise.reject(new SyntaxError("x")) })],
  ])("means nothing to start for %s", async (_n, impl) => {
    expect(await loadClientConfig(vi.fn(impl) as unknown as typeof fetch)).toEqual({});
  });
});

describe("error reporting", () => {
  it("starts nothing without a DSN", () => {
    expect(startErrorReporting(undefined)).toBe(false);
    expect(startErrorReporting({ dsn: "" })).toBe(false);
    reportError(new Error("x"));
    expect(Sentry.init).not.toHaveBeenCalled();
    expect(Sentry.captureException).not.toHaveBeenCalled();
  });

  it("starts once, with every data class off, and then reports", () => {
    expect(startErrorReporting({ dsn: "https://k@h/1", environment: "production", release: "r1" })).toBe(true);
    expect(startErrorReporting({ dsn: "https://k@h/1" })).toBe(true);
    expect(Sentry.init).toHaveBeenCalledTimes(1);
    const opts = vi.mocked(Sentry.init).mock.calls[0]![0]!;
    expect(opts).toMatchObject({ dsn: "https://k@h/1", environment: "production", release: "r1" });
    expect(opts.dataCollection).toMatchObject({ userInfo: false, cookies: false, httpHeaders: false, httpBodies: [], urlQueryParams: false, stackFrameVariables: false });

    const err = new Error("boom");
    reportError(err, { componentStack: "at X" });
    expect(Sentry.captureException).toHaveBeenCalledWith(err, { extra: { componentStack: "at X" } });
  });

  it("strips the query string from the reported URL but keeps the route in the hash", () => {
    startErrorReporting({ dsn: "https://k@h/1" });
    const beforeSend = vi.mocked(Sentry.init).mock.calls[0]![0]!.beforeSend!;
    const out = beforeSend({ request: { url: "https://app.7ar.chat/?next=%2Fsecret&x=1#/p/abc" } } as never, {} as never) as { request: { url: string } };
    expect(out.request.url).toBe("https://app.7ar.chat/#/p/abc");
  });
});

describe("startAnalytics", () => {
  it("adds one deferred Umami script with the website id, no query strings and do-not-track honoured", () => {
    const cfg = { scriptUrl: "https://umami.example/script.js", websiteId: "site-1" };
    expect(startAnalytics(cfg)).toBe(true);
    expect(startAnalytics(cfg)).toBe(true);
    const tags = document.head.querySelectorAll("script");
    expect(tags).toHaveLength(1);
    const s = tags[0]!;
    expect(s.getAttribute("src")).toBe("https://umami.example/script.js");
    expect(s.defer).toBe(true);
    expect(s.dataset).toMatchObject({ websiteId: "site-1", excludeSearch: "true", doNotTrack: "true" });
  });

  it("adds nothing without a complete config", () => {
    expect(startAnalytics(undefined)).toBe(false);
    expect(startAnalytics({ scriptUrl: "https://u/script.js", websiteId: "" })).toBe(false);
    expect(document.head.querySelectorAll("script")).toHaveLength(0);
  });
});

describe("identifyUser", () => {
  it("identifies by login only, once", () => {
    const identify = vi.fn();
    window.umami = { identify, track: vi.fn() };
    identifyUser("ghazi");
    identifyUser("ghazi");
    expect(identify).toHaveBeenCalledTimes(1);
    expect(identify).toHaveBeenCalledWith("ghazi");
  });

  it("waits for a tracker that loads late", () => {
    vi.useFakeTimers();
    const identify = vi.fn();
    identifyUser("alice");
    expect(identify).not.toHaveBeenCalled();
    vi.advanceTimersByTime(450);
    window.umami = { identify, track: vi.fn() };
    vi.advanceTimersByTime(200);
    expect(identify).toHaveBeenCalledWith("alice");
  });

  it("gives up quietly when statistics are off, and ignores a missing login", () => {
    vi.useFakeTimers();
    identifyUser("bob");
    identifyUser(undefined);
    vi.advanceTimersByTime(60_000);
    expect(vi.getTimerCount()).toBe(0);
  });
});
