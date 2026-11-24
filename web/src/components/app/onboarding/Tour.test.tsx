import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Tour, type TourStep } from "./Tour";

const steps: TourStep[] = [
  { id: "a", target: "#a", title: "First part", text: "About a." },
  { id: "b", title: "Second part", text: "About b." },
];

const setup = (onClose = vi.fn()) => {
  render(
    <div>
      <button type="button">before the tour</button>
      <div id="a">target</div>
      <Tour open steps={steps} onClose={onClose} />
    </div>,
  );
  return onClose;
};

describe("Tour", () => {
  it("is a labelled modal dialog, focused on its Next button", () => {
    setup();
    const d = screen.getByRole("dialog");
    expect(d.getAttribute("aria-modal")).toBe("true");
    expect(screen.getByRole("heading", { name: "First part" })).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Next" }));
  });

  it("moves with the buttons and the arrow keys, and finishes with Done", () => {
    const onClose = setup();
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("heading", { name: "Second part" })).toBeTruthy();
    fireEvent.keyDown(window, { key: "ArrowLeft" });
    expect(screen.getByRole("heading", { name: "First part" })).toBeTruthy();
    fireEvent.keyDown(window, { key: "ArrowRight" });
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(onClose).toHaveBeenCalledWith(true);
  });

  it("Escape and Skip leave the tour without completing it", () => {
    const onClose = setup();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onClose).toHaveBeenLastCalledWith(false);
    fireEvent.click(screen.getByRole("button", { name: "Skip tour" }));
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it("keeps Tab inside the card", () => {
    setup();
    const next = screen.getByRole("button", { name: "Next" });
    next.focus();
    fireEvent.keyDown(window, { key: "Tab" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Skip tour" }));
  });

  it("calls a step's before hook to make its target exist", () => {
    const before = vi.fn();
    render(<Tour open steps={[{ id: "x", title: "T", text: "t", before }]} onClose={() => {}} />);
    expect(before).toHaveBeenCalled();
  });

  it("renders nothing when closed", () => {
    render(<Tour open={false} steps={steps} onClose={() => {}} />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
