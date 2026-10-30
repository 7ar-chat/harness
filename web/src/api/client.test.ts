import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { ApiError, api, authHeaders, canWrite, downloadFile, downloadText, getDevUser, isOwner, isUnsupported, putBytes, setDevUser } from "./client";

let unauthorized: Mock<() => void>;
beforeEach(() => {
  unauthorized = vi.fn<() => void>();
  window.addEventListener("harness:unauthorized", unauthorized);
});
afterEach(() => window.removeEventListener("harness:unauthorized", unauthorized));

describe("request building", () => {
  it("prefixes /api, sends same-origin credentials and no body or content type on GET", async () => {
    // Arrange
    const f = mockFetch({ "GET /api/me": { user: { login: "a" } } });
    // Act
    const me = await api.get("/me");
    // Assert
    expect(me).toEqual({ user: { login: "a" } });
    expect(f.fn).toHaveBeenCalledWith("/api/me", expect.objectContaining({ method: "GET", credentials: "same-origin", body: undefined }));
    expect(f.calls[0]?.headers).not.toHaveProperty("Content-Type");
  });

  it("serialises the body as JSON with a content type", async () => {
    const f = mockFetch({ "POST /api/projects": { id: "p1" } });
    await api.post("/projects", { name: "x" });
    expect(f.calls[0]).toMatchObject({ method: "POST", headers: { "Content-Type": "application/json" }, body: { name: "x" } });
  });

  it("sends an empty object for body-less POST, PUT and PATCH, and none for DELETE", async () => {
    const f = mockFetch({ "POST /api/a": {}, "PUT /api/a": {}, "PATCH /api/a": {}, "DELETE /api/a": reply(204) });
    await api.post("/a");
    await api.put("/a");
    await api.patch("/a");
    await api.del("/a");
    expect(f.calls.map((c) => [c.method, c.body])).toEqual([["POST", {}], ["PUT", {}], ["PATCH", {}], ["DELETE", undefined]]);
  });

  it("adds the dev user header only while one is set", async () => {
    const f = mockFetch({ "GET /api/me": {} });
    await api.get("/me");
    setDevUser("bob");
    await api.get("/me");
    setDevUser("");
    await api.get("/me");
    expect(f.calls.map((c) => c.headers["X-Harness-User"])).toEqual([undefined, "bob", undefined]);
    expect(getDevUser()).toBe("");
  });
});

describe("dev user storage", () => {
  it("round-trips and tolerates unavailable storage", () => {
    setDevUser("carol");
    expect(getDevUser()).toBe("carol");
    expect(authHeaders()).toEqual({ "X-Harness-User": "carol" });
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("denied");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("denied");
    });
    expect(getDevUser()).toBe("");
    expect(() => setDevUser("x")).not.toThrow();
  });
});

describe("response handling", () => {
  it("returns undefined for 204", async () => {
    mockFetch({ "DELETE /api/x": reply(204) });
    await expect(api.del("/x")).resolves.toBeUndefined();
  });

  it("maps the error envelope to ApiError status, code and message", async () => {
    mockFetch({ "GET /api/x": reply(403, { error: { code: "forbidden", message: "Not your project." } }) });
    const e = await api.get("/x").catch((x: unknown) => x);
    expect(e).toBeInstanceOf(ApiError);
    expect(e).toMatchObject({ status: 403, code: "forbidden", message: "Not your project." });
  });

  it("falls back to the status text, then to the status, when the body is not an error envelope", async () => {
    mockFetch({ "GET /api/a": reply(502, "<html>bad gateway</html>", { statusText: "Bad Gateway" }), "GET /api/b": reply(500, "") });
    await expect(api.get("/a")).rejects.toMatchObject({ status: 502, code: "error", message: "Bad Gateway" });
    await expect(api.get("/b")).rejects.toMatchObject({ status: 500, code: "error", message: "HTTP 500" });
  });

  it("reports an expired session once per call without reading the body", async () => {
    mockFetch({ "GET /api/me": reply(401, { error: { code: "x", message: "y" } }) });
    await expect(api.get("/me")).rejects.toMatchObject({ status: 401, code: "unauthenticated" });
    expect(unauthorized).toHaveBeenCalledTimes(1);
  });

  it("does not treat other failures as an expired session", async () => {
    mockFetch({ "GET /api/me": reply(500, {}) });
    await api.get("/me").catch(() => undefined);
    expect(unauthorized).not.toHaveBeenCalled();
  });
});

describe("predicates", () => {
  it("isUnsupported matches only 404 and 501 ApiErrors", () => {
    expect(isUnsupported(new ApiError(404, "x", "m"))).toBe(true);
    expect(isUnsupported(new ApiError(501, "x", "m"))).toBe(true);
    expect(isUnsupported(new ApiError(500, "x", "m"))).toBe(false);
    expect(isUnsupported(new Error("404"))).toBe(false);
    expect(isUnsupported(undefined)).toBe(false);
  });
  it("canWrite allows owners, members and unknown roles; isOwner only owners", () => {
    expect([canWrite(undefined), canWrite("owner"), canWrite("member"), canWrite("viewer")]).toEqual([true, true, true, false]);
    expect([isOwner(undefined), isOwner("owner"), isOwner("member"), isOwner("viewer")]).toEqual([false, true, false, false]);
  });
});

describe("putBytes", () => {
  it("uploads the blob with its own content type", async () => {
    const f = mockFetch({ "PUT /api/me/avatar": { login: "a" } });
    const blob = new Blob(["png"], { type: "image/png" });
    await expect(putBytes("/me/avatar", blob)).resolves.toEqual({ login: "a" });
    expect(f.fn).toHaveBeenCalledWith("/api/me/avatar", expect.objectContaining({ method: "PUT", body: blob, headers: expect.objectContaining({ "Content-Type": "image/png" }) }));
  });
  it("maps errors, tolerating a non-JSON body", async () => {
    mockFetch({ "PUT /api/a": reply(413, { error: { code: "too_large", message: "Too big." } }), "PUT /api/b": reply(500, "oops") });
    await expect(putBytes("/a", new Blob(["x"]))).rejects.toMatchObject({ status: 413, code: "too_large", message: "Too big." });
    await expect(putBytes("/b", new Blob(["x"]))).rejects.toMatchObject({ status: 500, code: "error", message: "HTTP 500" });
  });
  it("signals an expired session", async () => {
    mockFetch({ "PUT /api/a": reply(401) });
    await expect(putBytes("/a", new Blob(["x"]))).rejects.toMatchObject({ status: 401 });
    expect(unauthorized).toHaveBeenCalledTimes(1);
  });
});

describe("downloads", () => {
  let click: ReturnType<typeof vi.spyOn>;
  let names: string[];
  beforeEach(() => {
    names = [];
    click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      names.push(this.download);
    });
    URL.createObjectURL = vi.fn(() => "blob:x");
    URL.revokeObjectURL = vi.fn();
  });

  it("downloadFile encodes the path and saves under the given name", async () => {
    const f = mockFetch({ "GET /api/threads/t1/files/download?path=a%20b%2Fc%26d.txt": reply(200, "data") });
    await downloadFile("t1", "a b/c&d.txt", "c&d.txt");
    expect(f.calls).toHaveLength(1);
    expect(click).toHaveBeenCalledTimes(1);
    expect(names).toEqual(["c&d.txt"]);
    expect(document.querySelector("a[download]")).toBeNull();
  });

  it("downloadFile and downloadText turn a failed response into a download ApiError", async () => {
    mockFetch({ "GET /api/threads/t1/files/download?path=x": reply(404), "GET /api/projects/p1/memory/export": reply(500) });
    await expect(downloadFile("t1", "x", "x")).rejects.toMatchObject({ status: 404, code: "download", message: "Download failed." });
    await expect(downloadText("/projects/p1/memory/export", "MEMORY.md")).rejects.toMatchObject({ status: 500, code: "download", message: "Export failed." });
    expect(click).not.toHaveBeenCalled();
  });

  it("downloadText saves the endpoint's text", async () => {
    mockFetch({ "GET /api/projects/p1/memory/export": reply(200, "# memory") });
    await downloadText("/projects/p1/memory/export", "MEMORY.md");
    expect(names).toEqual(["MEMORY.md"]);
  });
});

describe("loginRedirect", () => {
  it("sends the browser to the login with the current location, only once", async () => {
    // Arrange: a fresh module, since the guard is module state
    vi.resetModules();
    const assign = vi.fn();
    vi.stubGlobal("location", { pathname: "/ui/", search: "?a=1", hash: "#/t/t 1", assign });
    const { loginRedirect } = await import("./client");
    // Act
    loginRedirect();
    loginRedirect();
    // Assert
    expect(assign).toHaveBeenCalledTimes(1);
    expect(assign).toHaveBeenCalledWith(`/auth/login?next=${encodeURIComponent("/ui/?a=1#/t/t 1")}`);
  });
});
