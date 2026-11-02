import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Interrupt } from "@/lib/agui";
import { InterruptCard } from "./Prompts";

const setup = (interrupt: Interrupt, over: { busy?: boolean; disabled?: boolean } = {}) => {
  const resume = vi.fn<(b: { interruptId: string; reason: string; decision?: "approve" | "deny"; always?: boolean; text?: string }) => Promise<void>>(async () => {});
  render(<InterruptCard interrupt={interrupt} resume={resume} busy={over.busy ?? false} disabled={over.disabled ?? false} />);
  return resume;
};

describe("approval card", () => {
  const approval: Interrupt = { id: "i1", reason: "tool_approval", approval: { id: "a1", toolName: "bash", toolInputPreview: "rm -rf build" } };

  it("shows the tool and its input", () => {
    setup(approval);
    expect(screen.getByRole("alertdialog", { name: "Approval needed" })).toBeTruthy();
    expect(screen.getByText("bash")).toBeTruthy();
    expect(screen.getByText("rm -rf build")).toBeTruthy();
  });

  it("answers with the approval id (falling back to the interrupt id), once, always, or deny", () => {
    const resume = setup(approval);
    fireEvent.click(screen.getByRole("button", { name: /^Approve$/ }));
    fireEvent.click(screen.getByRole("button", { name: "Approve always" }));
    fireEvent.click(screen.getByRole("button", { name: /Deny/ }));
    expect(resume.mock.calls.map((c) => c[0])).toEqual([
      { interruptId: "a1", reason: "tool_approval", decision: "approve", always: false },
      { interruptId: "a1", reason: "tool_approval", decision: "approve", always: true },
      { interruptId: "a1", reason: "tool_approval", decision: "deny", always: false },
    ]);
  });

  it("uses the interrupt id when the approval carries none, and the message when the tool is unnamed", () => {
    const resume = setup({ id: "i9", reason: "tool_approval", message: "write_file" });
    expect(screen.getByText("write_file")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^Approve$/ }));
    expect(resume.mock.calls[0]?.[0]).toMatchObject({ interruptId: "i9" });
  });

  it("cannot be answered while busy or when the viewer may not", () => {
    for (const over of [{ busy: true }, { disabled: true }]) {
      const resume = setup(approval, over);
      for (const b of screen.getAllByRole("button")) expect((b as HTMLButtonElement).disabled).toBe(true);
      expect(resume).not.toHaveBeenCalled();
      document.body.innerHTML = "";
    }
  });
});

describe("question card", () => {
  it("answers with a chosen option", () => {
    const resume = setup({ id: "q1", reason: "ask_user", message: "Which?", responseSchema: { options: [{ label: "A" }, { label: "B", description: "second" }], allowFreeText: false } });
    expect(screen.getByText("Which?")).toBeTruthy();
    expect(screen.queryByLabelText("Your answer")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "B" }));
    expect(resume).toHaveBeenCalledWith({ interruptId: "q1", reason: "ask_user", text: "B" });
  });

  it("sends a trimmed free-text answer and ignores a blank one", () => {
    const resume = setup({ id: "q1", reason: "ask_user" });
    expect(screen.getByText("The agent has a question")).toBeTruthy();
    const input = screen.getByLabelText("Your answer");
    const submit = screen.getByRole("button", { name: "Answer" }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.change(input, { target: { value: "   " } });
    expect(submit.disabled).toBe(true);
    fireEvent.change(input, { target: { value: "  go 1.27 " } });
    fireEvent.click(submit);
    expect(resume).toHaveBeenCalledTimes(1);
    expect(resume).toHaveBeenCalledWith({ interruptId: "q1", reason: "ask_user", text: "go 1.27" });
  });

  it("names who is being asked", () => {
    render(<InterruptCard interrupt={{ id: "q", reason: "ask_user" }} resume={async () => {}} busy={false} disabled={false} askedUser="Bob" />);
    expect(screen.getByText("Asking Bob")).toBeTruthy();
  });
});
