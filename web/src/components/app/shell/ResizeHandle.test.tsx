import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it } from "vitest";
import { clamp, ResizeHandle } from "./ResizeHandle";

function Host({ orientation }: { orientation: "vertical" | "horizontal" }) {
  const [v, setV] = useState(272);
  return <ResizeHandle orientation={orientation} label="Resize" value={v} min={200} max={300} initial={272} onChange={setV} />;
}
const now = () => Number(screen.getByRole("separator", { name: "Resize" }).getAttribute("aria-valuenow"));

describe("ResizeHandle", () => {
  it("clamps", () => expect([clamp(1, 2, 5), clamp(9, 2, 5), clamp(3, 2, 5)]).toEqual([2, 5, 3]));

  it("moves with the arrow keys inside its limits and resets on double-click", () => {
    render(<Host orientation="vertical" />);
    const h = screen.getByRole("separator", { name: "Resize" });
    fireEvent.keyDown(h, { key: "ArrowRight" });
    expect(now()).toBe(288);
    fireEvent.keyDown(h, { key: "ArrowRight", shiftKey: true });
    expect(now()).toBe(300);
    fireEvent.keyDown(h, { key: "Home" });
    expect(now()).toBe(200);
    fireEvent.doubleClick(h);
    expect(now()).toBe(272);
  });

  it("grows a bottom panel upwards", () => {
    render(<Host orientation="horizontal" />);
    const h = screen.getByRole("separator", { name: "Resize" });
    fireEvent.keyDown(h, { key: "ArrowUp" });
    expect(now()).toBe(288);
    fireEvent.keyDown(h, { key: "ArrowDown" });
    fireEvent.keyDown(h, { key: "ArrowDown" });
    expect(now()).toBe(256);
  });

  it("follows a pointer drag", () => {
    render(<Host orientation="vertical" />);
    const h = screen.getByRole("separator", { name: "Resize" });
    fireEvent.pointerDown(h, { clientX: 100, pointerId: 1 });
    fireEvent.pointerMove(h, { clientX: 120, pointerId: 1 });
    expect(now()).toBe(292);
    fireEvent.pointerUp(h, { pointerId: 1 });
    fireEvent.pointerMove(h, { clientX: 10, pointerId: 1 });
    expect(now()).toBe(292);
  });
});
