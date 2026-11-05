import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const docs = [
  { name: "cv.pdf", size: 120_000, mimeType: "application/pdf", modified: "2026-11-01T10:00:00Z" },
  { name: "letter.docx", size: 30_000, mimeType: "application/x-docx", modified: "2026-11-04T10:00:00Z" },
];
const page = () => within(screen.getByRole("main"));

describe("ReferencesPage", () => {
  it("lists the files flat, newest first, and filters them", async () => {
    mockFetch({ ...shellRoutes(), "GET /api/me/documents": docs });
    await renderApp("/references");
    const table = await page().findByRole("table", { name: "Your references" });
    const names = within(table).getAllByRole("row").slice(1).map((r) => within(r).getByText(/\.(pdf|docx)$/).textContent);
    expect(names).toEqual(["letter.docx", "cv.pdf"]);
    fireEvent.change(page().getByRole("searchbox", { name: "Search references" }), { target: { value: "pdf" } });
    expect(within(table).queryByText("letter.docx")).toBeNull();
    expect(page().getByText(/2 of 50 files/)).toBeTruthy();
  });

  it("deletes after a confirmation", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/me/documents": docs, "DELETE /api/me/documents/cv.pdf": reply(204) });
    await renderApp("/references");
    fireEvent.click(await page().findByRole("button", { name: "Delete cv.pdf" }));
    expect(m.find("DELETE", "/api/me/documents/cv.pdf")).toHaveLength(0);
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(m.find("DELETE", "/api/me/documents/cv.pdf")).toHaveLength(1));
  });

  it("says so when the server keeps no library", async () => {
    mockFetch({ ...shellRoutes(), "GET /api/me/documents": reply(404) });
    await renderApp("/references");
    expect(await page().findByText("This server keeps no reference library.")).toBeTruthy();
  });
});
