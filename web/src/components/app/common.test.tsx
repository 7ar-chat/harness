import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "@/api/client";
import { ErrorBoundary } from "./ErrorBoundary";
import { ErrorState, isActive, needsYou } from "./common";

describe("status helpers", () => {
  it("needsYou is exactly the states that wait on a person or failed; isActive is everything but idle", () => {
    expect((["idle", "queued", "running", "waiting_approval", "waiting_input", "failed"] as const).map(needsYou)).toEqual([false, false, false, true, true, true]);
    expect((["idle", "queued", "running", "waiting_approval", "waiting_input", "failed"] as const).map(isActive)).toEqual([false, true, true, true, true, true]);
  });
});

describe("ErrorState", () => {
  it.each([
    [403, "You do not have access to this", false],
    [404, "Not found", false],
    [500, "Something went wrong", true],
  ])("for HTTP %i shows %s (retry offered: %s)", (status, title, retry) => {
    render(<ErrorState error={new ApiError(status, "x", "server said no")} retry={() => {}} />);
    expect(screen.getByText(title)).toBeTruthy();
    expect(!!screen.queryByRole("button", { name: "Try again" })).toBe(retry);
  });

  it("explains a 404 in its own words and shows plain error messages and strings otherwise", () => {
    const { unmount } = render(<ErrorState error={new ApiError(404, "x", "internal detail")} />);
    expect(screen.queryByText("internal detail")).toBeNull();
    expect(screen.getByText(/may have been deleted/)).toBeTruthy();
    unmount();
    render(<ErrorState error="just text" />);
    expect(screen.getByText("just text")).toBeTruthy();
  });

  it("calls retry when asked", () => {
    const retry = vi.fn();
    render(<ErrorState error={new Error("x")} retry={retry} />);
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(retry).toHaveBeenCalledTimes(1);
  });
});

describe("ErrorBoundary", () => {
  const Boom = ({ boom }: { boom: boolean }) => {
    if (boom) throw new Error("render failed");
    return <p>fine</p>;
  };

  it("catches a render error and recovers when the route changes", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const { rerender } = render(
      <ErrorBoundary reset="/a">
        <Boom boom />
      </ErrorBoundary>,
    );
    expect(screen.getByText("render failed")).toBeTruthy();
    rerender(
      <ErrorBoundary reset="/b">
        <Boom boom={false} />
      </ErrorBoundary>,
    );
    expect(screen.getByText("fine")).toBeTruthy();
  });

  it("stays on the error when the route did not change", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const { rerender } = render(
      <ErrorBoundary reset="/a">
        <Boom boom />
      </ErrorBoundary>,
    );
    rerender(
      <ErrorBoundary reset="/a">
        <Boom boom={false} />
      </ErrorBoundary>,
    );
    expect(screen.getByText("render failed")).toBeTruthy();
  });
});
