import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Welcome } from "./Welcome";

const actions = () => ({ newProject: vi.fn(), startSession: vi.fn(), trySample: vi.fn() });

describe("Welcome", () => {
  it("explains the four ideas one by one, then offers the first actions", () => {
    render(<Welcome open onClose={() => {}} actions={actions()} />);
    for (const idea of ["Project", "Session", "Task and requirement", "Needs you"]) {
      expect(screen.getByRole("heading", { name: idea })).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: /Next|Where to start/ }));
    }
    expect(screen.getByRole("button", { name: /Try it with a sample/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Create or import a project/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Start a session/ })).toBeTruthy();
  });

  it("can be skipped at any step", () => {
    const onClose = vi.fn();
    render(<Welcome open onClose={onClose} actions={actions()} />);
    fireEvent.click(screen.getByRole("button", { name: "Skip for now" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("goes back, and a first action closes the welcome and runs", () => {
    const onClose = vi.fn();
    const a = actions();
    render(<Welcome open onClose={onClose} actions={a} />);
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    expect(screen.getByRole("heading", { name: "Project" })).toBeTruthy();
    for (let i = 0; i < 4; i++) fireEvent.click(screen.getByRole("button", { name: /Next|Where to start/ }));
    fireEvent.click(screen.getByRole("button", { name: /Try it with a sample/ }));
    expect(onClose).toHaveBeenCalled();
    expect(a.trySample).toHaveBeenCalled();
  });

  it("renders nothing when closed", () => {
    render(<Welcome open={false} onClose={() => {}} actions={actions()} />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
