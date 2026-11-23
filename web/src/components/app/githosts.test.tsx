import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { GitHost, GitHostList } from "@/api/generated";
import { mockFetch, type Call } from "@/test/fetch";
import { me, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const host = (extra: Partial<GitHost> = {}): GitHost => ({
  name: "gitlab.example.com",
  kind: "gitlab",
  baseUrl: "https://gitlab.example.com",
  knownHosts: ["gitlab.example.com ssh-ed25519 AAAA"],
  api: true,
  hasServiceToken: false,
  egress: ["gitlab.example.com"],
  egressBlocked: [],
  credential: { hasToken: false, hasSshKey: false },
  ...extra,
});
const list = (hosts: GitHost[], admin = false): GitHostList => ({ enabled: true, hosts, admin });
const admin = () => ({ ...me(), admin: true });
const page = () => within(screen.getByRole("main"));

describe("Profile, Git access", () => {
  it("saves a token for a host without ever showing one back", async () => {
    // Arrange
    let h = host();
    const m = mockFetch({
      ...shellRoutes(),
      "GET /api/connectors": [],
      "GET /api/git-hosts": () => list([h]),
      "PUT /api/me/git-hosts/gitlab.example.com/token": () => {
        h = host({ credential: { hasToken: true, username: "ann", hasSshKey: false } });
        return new Response(null, { status: 204 });
      },
    });
    // Act
    await renderApp("/profile");
    const item = within(await page().findByRole("listitem", { name: "gitlab.example.com" }));
    fireEvent.change(item.getByLabelText("Personal access token"), { target: { value: "glpat-secret" } });
    fireEvent.change(item.getByLabelText("User name on gitlab.example.com"), { target: { value: "ann" } });
    fireEvent.click(item.getByRole("button", { name: "Save token" }));
    // Assert
    await waitFor(() => expect(m.find("PUT", "/api/me/git-hosts/gitlab.example.com/token")).toHaveLength(1));
    expect(m.find("PUT", "/api/me/git-hosts/gitlab.example.com/token")[0]?.body).toEqual({ token: "glpat-secret", username: "ann" });
    expect(await item.findByText(/A token is saved for ann/)).toBeTruthy();
    expect(document.body.textContent).not.toContain("glpat-secret");
  });

  it("generates an SSH key and shows its public half", async () => {
    // Arrange
    let h = host();
    const m = mockFetch({
      ...shellRoutes(),
      "GET /api/connectors": [],
      "GET /api/git-hosts": () => list([h]),
      "POST /api/me/git-hosts/gitlab.example.com/ssh-key": () => {
        h = host({ credential: { hasToken: false, hasSshKey: true, publicKey: "ssh-ed25519 AAAAPUB harness:alice", fingerprint: "SHA256:abc" } });
        return { publicKey: "ssh-ed25519 AAAAPUB harness:alice", fingerprint: "SHA256:abc" };
      },
    });
    // Act
    await renderApp("/profile");
    fireEvent.click(await page().findByRole("button", { name: /Generate an SSH key/ }));
    // Assert
    await waitFor(() => expect(m.find("POST", "/api/me/git-hosts/gitlab.example.com/ssh-key")).toHaveLength(1));
    expect(((await page().findByLabelText("Public key for gitlab.example.com")) as HTMLTextAreaElement).value).toContain("ssh-ed25519 AAAAPUB");
    expect(page().getByText("SHA256:abc")).toBeTruthy();
  });

  it("is absent when the server has the feature off", async () => {
    // Arrange
    mockFetch({ ...shellRoutes(), "GET /api/connectors": [] });
    // Act
    await renderApp("/profile");
    await page().findByRole("heading", { name: "Settings", level: 1 });
    // Assert
    expect(screen.queryByText("Git access")).toBeNull();
  });
});

describe("Admin, Git hosts", () => {
  it("registers a host with its SSH host keys", async () => {
    // Arrange
    let hosts: GitHost[] = [];
    const m = mockFetch({
      ...shellRoutes(undefined, admin()),
      "GET /api/admin/users?status=pending": [],
      "GET /api/git-hosts": () => list(hosts, true),
      "POST /api/admin/git-hosts": (c: Call) => {
        const b = c.body as { name: string; kind: string; knownHosts: string[] };
        const h = host({ name: b.name, kind: b.kind, knownHosts: b.knownHosts });
        hosts = [h];
        return h;
      },
    });
    // Act
    await renderApp("/admin");
    fireEvent.mouseDown(await screen.findByRole("tab", { name: "Git hosts" }));
    fireEvent.click(await screen.findByRole("button", { name: /Add a git host/ }));
    fireEvent.change(screen.getByLabelText("Host name"), { target: { value: "gitea.example.com" } });
    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "gitea" } });
    fireEvent.change(screen.getByLabelText("SSH host keys"), { target: { value: "gitea.example.com ssh-ed25519 KEY\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Add host" }));
    // Assert
    await waitFor(() => expect(m.find("POST", "/api/admin/git-hosts")).toHaveLength(1));
    expect(m.find("POST", "/api/admin/git-hosts")[0]?.body).toMatchObject({ name: "gitea.example.com", kind: "gitea", knownHosts: ["gitea.example.com ssh-ed25519 KEY"] });
    expect(await screen.findByRole("listitem", { name: "gitea.example.com" })).toBeTruthy();
  });

  it("warns when the sandbox allowlist does not let a registered host through", async () => {
    // Arrange
    mockFetch({
      ...shellRoutes(undefined, admin()),
      "GET /api/admin/users?status=pending": [],
      "GET /api/git-hosts": list([host({ egressBlocked: ["gitlab.example.com"] })], true),
    });
    // Act
    await renderApp("/admin");
    fireEvent.mouseDown(await screen.findByRole("tab", { name: "Git hosts" }));
    // Assert
    expect((await screen.findByRole("alert")).textContent).toContain("SANDBOX_EGRESS_ALLOWLIST does not include gitlab.example.com");
  });

  it("removes a host", async () => {
    // Arrange
    const m = mockFetch({
      ...shellRoutes(undefined, admin()),
      "GET /api/admin/users?status=pending": [],
      "GET /api/git-hosts": list([host()], true),
      "DELETE /api/admin/git-hosts/gitlab.example.com": new Response(null, { status: 204 }),
    });
    // Act
    await renderApp("/admin");
    fireEvent.mouseDown(await screen.findByRole("tab", { name: "Git hosts" }));
    fireEvent.click(await screen.findByRole("button", { name: /Remove/ }));
    // Assert
    await waitFor(() => expect(m.find("DELETE", "/api/admin/git-hosts/gitlab.example.com")).toHaveLength(1));
  });
});
