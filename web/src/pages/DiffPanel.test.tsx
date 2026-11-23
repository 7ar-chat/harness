import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Message } from "@/api/types";
import { DiffPanel, sessionEdits } from "./DiffPanel";

const call = (name: string, args: unknown) => ({ id: "c", type: "function", function: { name, arguments: JSON.stringify(args) } });
const msg = (toolCalls: Message["toolCalls"]): Message => ({ id: "m", role: "assistant", content: "", toolCalls, createdAt: "2026-01-01T00:00:00Z" });

describe("DiffPanel", () => {
  it("collects the edit_file calls of a session and ignores other tools", () => {
    const edits = sessionEdits([msg([call("edit_file", { path: "a.go", oldText: "x", newText: "y" }), call("read_file", { path: "a.go" })]), msg([call("edit_file", { path: "b.go", oldText: "1", newText: "2" })])]);
    expect(edits.map((e) => e.path)).toEqual(["a.go", "b.go"]);
  });

  it("says there are no edits yet when the agent changed nothing", () => {
    render(<DiffPanel messages={[msg([call("read_file", { path: "a.go" })])]} />);
    expect(screen.getByText("No edits yet")).toBeTruthy();
  });

  it("lists one section per edit, named by its file", () => {
    render(<DiffPanel messages={[msg([call("edit_file", { path: "charges/handler.go", oldText: "a", newText: "b" })])]} />);
    expect(screen.getByRole("region", { name: "Edit of charges/handler.go" })).toBeTruthy();
  });
});
