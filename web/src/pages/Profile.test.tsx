import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { describe, expect, it, vi } from "vitest";
import type { Connector } from "@/api/types";
import { mockFetch, reply } from "@/test/fetch";
import { me, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const connectors: Connector[] = [
  { kind: "github", configured: true, connected: true, account: "alice-gh" },
  { kind: "telegram", configured: false, connected: false, message: "Set TELEGRAM_BOT_TOKEN to enable it." },
];
const withUser = (extra: Partial<ReturnType<typeof me>["user"]> = {}) => {
  const m = me();
  return { ...m, user: { ...m.user, ...extra } };
};
const routes = (extra: Record<string, unknown> = {}, m = me()) => ({ ...shellRoutes(undefined, m), "GET /api/connectors": connectors, ...extra });
const page = () => within(screen.getByRole("main"));

describe("ProfilePage", () => {
  it("shows who you are with the connectors of the server", async () => {
    // Arrange
    mockFetch(routes());
    // Act
    await renderApp("/profile");
    // Assert
    expect(await page().findByRole("heading", { name: "Profile", level: 1 })).toBeTruthy();
    expect(await page().findByText("alice@example.com")).toBeTruthy();
    expect((page().getByLabelText("Display name") as HTMLInputElement).placeholder).toBe("Alice Doe");
    expect(await page().findByText("Connected as alice-gh")).toBeTruthy();
    expect(page().getByRole("button", { name: /Import a project from GitHub/ })).toBeTruthy();
    expect(page().getByText("Not configured")).toBeTruthy();
    expect(page().getByText("Set TELEGRAM_BOT_TOKEN to enable it.")).toBeTruthy();
  });

  it("says so when the server has no connectors", async () => {
    // Arrange
    mockFetch(routes({ "GET /api/connectors": [] }));
    // Act
    await renderApp("/profile");
    // Assert
    expect(await page().findByText("No connectors are available on this server.")).toBeTruthy();
  });

  it("treats a server without connectors (404) as an empty list", async () => {
    // Arrange
    mockFetch(routes({ "GET /api/connectors": reply(404) }));
    // Act
    await renderApp("/profile");
    // Assert
    expect(await page().findByText("No connectors are available on this server.")).toBeTruthy();
  });

  it("saves the display name with PATCH /me/profile and refreshes the user", async () => {
    // Arrange
    let saved = "";
    const f = mockFetch(
      routes({
        "PATCH /api/me/profile": (c: { body: unknown }) => {
          saved = (c.body as { displayName: string }).displayName;
          return { ...me().user, displayName: saved };
        },
        "GET /api/me": () => withUser({ displayName: saved }),
      }),
    );
    await renderApp("/profile");
    const input = (await page().findByLabelText("Display name")) as HTMLInputElement;
    const save = page().getByRole("button", { name: "Save" });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    // Act
    fireEvent.change(input, { target: { value: "  Ali  " } });
    fireEvent.click(save);
    // Assert
    await waitFor(() => expect(f.find("PATCH", "/api/me/profile")).toHaveLength(1));
    expect(f.find("PATCH", "/api/me/profile")[0]?.body).toEqual({ displayName: "Ali" });
    expect(await page().findByRole("button", { name: "Reset to default" })).toBeTruthy();
    expect((page().getByLabelText("Display name") as HTMLInputElement).value).toBe("Ali");
  });

  it("resets a custom display name by sending an empty one", async () => {
    // Arrange
    const f = mockFetch(routes({ "PATCH /api/me/profile": withUser().user }, withUser({ displayName: "Ali" })));
    await renderApp("/profile");
    // Act
    fireEvent.click(await page().findByRole("button", { name: "Reset to default" }));
    // Assert
    await waitFor(() => expect(f.find("PATCH", "/api/me/profile")).toHaveLength(1));
    expect(f.find("PATCH", "/api/me/profile")[0]?.body).toEqual({ displayName: "" });
  });

  it("reports a failed save and keeps what was typed", async () => {
    // Arrange
    const err = vi.spyOn(toast, "error").mockImplementation(() => "");
    mockFetch(routes({ "PATCH /api/me/profile": reply(500, { error: { code: "internal", message: "database is down" } }) }));
    await renderApp("/profile");
    const input = (await page().findByLabelText("Display name")) as HTMLInputElement;
    // Act
    fireEvent.change(input, { target: { value: "Ali" } });
    fireEvent.click(page().getByRole("button", { name: "Save" }));
    // Assert
    await waitFor(() => expect(err).toHaveBeenCalledWith("database is down"));
    expect(input.value).toBe("Ali");
  });
});
