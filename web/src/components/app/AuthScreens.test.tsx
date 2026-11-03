import { render, screen } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, shellRoutes } from "@/test/fixtures";
import { newQueryClient, renderApp } from "@/test/render";
import { LoginPage, providerHref } from "./AuthScreens";

const providers = { providers: [{ id: "oidc", name: "Single sign-on", url: "/auth/login" }, { id: "github", name: "GitHub", url: "/auth/github/login" }] };
const unauthorized = () => reply(401, { error: { code: "unauthenticated", message: "Authentication required." } });

describe("sign-in", () => {
  it("shows the login page with the providers when there is no session", async () => {
    // Arrange
    mockFetch({ "GET /api/me": unauthorized, "GET /auth/providers": providers });
    // Act
    render(<QueryClientProvider client={newQueryClient()}><LoginPage /></QueryClientProvider>);
    // Assert
    const gh = (await screen.findByRole("link", { name: /Continue with GitHub/ })) as HTMLAnchorElement;
    expect(gh.getAttribute("href")).toMatch(/^\/auth\/github\/login\?next=/);
    expect((screen.getByRole("link", { name: /single sign-on/ }) as HTMLAnchorElement).getAttribute("href")).toMatch(/^\/auth\/login\?next=/);
  });

  it("keeps the page to come back to in the link", () => {
    expect(providerHref({ id: "oidc", name: "x", url: "/auth/login" }, "/t/1?a=b#c")).toBe("/auth/login?next=%2Ft%2F1%3Fa%3Db%23c");
  });

  it("says so when no provider is configured", async () => {
    mockFetch({ "GET /auth/providers": { providers: [] } });
    render(<QueryClientProvider client={newQueryClient()}><LoginPage /></QueryClientProvider>);
    expect(await screen.findByText("Sign-in is not configured on this server.")).toBeTruthy();
  });

  it("offers a retry when the providers cannot be loaded", async () => {
    mockFetch({ "GET /auth/providers": () => reply(500, "boom") });
    render(<QueryClientProvider client={newQueryClient()}><LoginPage /></QueryClientProvider>);
    expect(await screen.findByRole("button", { name: "Retry" }, { timeout: 4000 })).toBeTruthy();
  });

  it("replaces the app by the login page when /me answers 401", async () => {
    // Arrange
    mockFetch({ "GET /api/me": unauthorized, "GET /auth/providers": providers });
    // Act
    await renderApp("/", undefined, { shell: false });
    // Assert
    expect(await screen.findByRole("link", { name: /Continue with GitHub/ })).toBeTruthy();
  });
});

describe("approval screens", () => {
  it("shows the waiting screen to a pending user", async () => {
    // Arrange
    const m = me();
    m.user.status = "pending";
    mockFetch(shellRoutes(undefined, m));
    // Act
    await renderApp("/", undefined, { shell: false });
    // Assert
    expect(await screen.findByText("Waiting for approval")).toBeTruthy();
    expect(screen.getByText("alice@example.com")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
    expect(screen.queryByText("Dashboard")).toBeNull();
  });

  it("shows access denied to a rejected user", async () => {
    // Arrange
    const m = me();
    m.user.status = "rejected";
    mockFetch(shellRoutes(undefined, m));
    // Act
    await renderApp("/", undefined, { shell: false });
    // Assert
    expect(await screen.findByText("Access denied")).toBeTruthy();
  });
});
