import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { AdminUser } from "@/api/types";
import { mockFetch } from "@/test/fetch";
import { me, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const T = "2026-01-01T00:00:00Z";
const pendingUser: AdminUser = { login: "github_carol", name: "Carol", email: "carol@example.com", avatarUrl: "", createdAt: T, status: "pending" };
const admin = () => ({ ...me(), admin: true });

describe("AdminPage", () => {
  it("lists the pending users and approves one", async () => {
    // Arrange
    let users = [pendingUser];
    const { find } = mockFetch({
      ...shellRoutes(undefined, admin()),
      "GET /api/admin/users?status=pending": () => users,
      "POST /api/admin/users/github_carol/approve": () => {
        users = [];
        return { ...pendingUser, status: "approved" };
      },
    });
    // Act
    await renderApp("/admin");
    const row = (await screen.findByText("Carol", { selector: "main span" })).closest("li")!;
    expect(within(row).getByText("GitHub")).toBeTruthy();
    fireEvent.click(within(row).getByRole("button", { name: /Approve/ }));
    // Assert
    await waitFor(() => expect(find("POST", "/api/admin/users/github_carol/approve")).toHaveLength(1));
    expect(await screen.findByText("Nobody is waiting for approval.")).toBeTruthy();
  });

  it("rejects a user from the list", async () => {
    // Arrange
    const { find } = mockFetch({
      ...shellRoutes(undefined, admin()),
      "GET /api/admin/users?status=pending": [pendingUser],
      "POST /api/admin/users/github_carol/reject": { ...pendingUser, status: "rejected" },
    });
    // Act
    await renderApp("/admin");
    fireEvent.click(await screen.findByRole("button", { name: /Reject/ }));
    // Assert
    await waitFor(() => expect(find("POST", "/api/admin/users/github_carol/reject")).toHaveLength(1));
  });

  it("shows the admin link with the pending count to administrators only", async () => {
    // Arrange
    mockFetch({ ...shellRoutes(undefined, admin()), "GET /api/admin/users?status=pending": [pendingUser] });
    // Act
    await renderApp("/");
    // Assert
    expect(await screen.findByLabelText("1 users awaiting approval")).toBeTruthy();
  });

  it("is forbidden for a non-admin", async () => {
    // Arrange
    const { find } = mockFetch(shellRoutes());
    // Act
    await renderApp("/admin");
    // Assert
    expect(await screen.findByText("Forbidden")).toBeTruthy();
    expect(find("GET", "/api/admin/users")).toHaveLength(0);
    expect(screen.queryByRole("link", { name: /Admin/ })).toBeNull();
  });
});
