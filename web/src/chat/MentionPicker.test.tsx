import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { addChip, applyMention, filterMentions, findMention, MentionChips, MentionPicker, type MentionItem } from "./MentionPicker";

const role = (id: string, detail?: string): MentionItem => ({ kind: "role", id, label: id, detail });
const skill = (id: string): MentionItem => ({ kind: "skill", id, label: id });
const req: MentionItem = { kind: "requirement", id: "REQ-7", label: "Login must be rate limited", detail: "REQ-7" };
const doc: MentionItem = { kind: "document", id: "cv.pdf", label: "cv.pdf" };

describe("findMention", () => {
  it("finds the @token the text ends with", () => {
    expect(findMention("@")).toEqual({ start: 0, query: "" });
    expect(findMention("@cod")).toEqual({ start: 0, query: "cod" });
    expect(findMention("please ask @rev")).toEqual({ start: 11, query: "rev" });
    expect(findMention("line one\n@pl")).toEqual({ start: 9, query: "pl" });
  });

  it("ignores an @ inside a word, an e-mail address and a finished mention", () => {
    expect(findMention("mail me at bob@example.com")).toBeNull();
    expect(findMention("@coder fix it")).toBeNull();
    expect(findMention("no mention")).toBeNull();
    expect(findMention("a @ b")).toBeNull();
  });
});

describe("filterMentions", () => {
  const items = [doc, skill("review"), role("reviewer", "reads a diff"), role("coder"), req];

  it("lists everything for a bare @, roles first, in the order roles, skills, requirements, documents", () => {
    expect(filterMentions(items, "").map((i) => i.kind)).toEqual(["role", "role", "skill", "requirement", "document"]);
  });

  it("puts names that start with the query before names that merely contain it", () => {
    const got = filterMentions([role("pre-review"), role("reviewer")], "rev");
    expect(got.map((i) => i.id)).toEqual(["reviewer", "pre-review"]);
  });

  it("matches the detail and is case-insensitive, and drops the rest", () => {
    expect(filterMentions(items, "DIFF").map((i) => i.id)).toEqual(["reviewer"]);
    expect(filterMentions(items, "zzz")).toEqual([]);
  });

  it("caps the list", () => {
    const many = Array.from({ length: 20 }, (_, i) => role(`r${i}`));
    expect(filterMentions(many, "", 5)).toHaveLength(5);
  });
});

describe("applyMention and addChip", () => {
  it("removes the @token for a role or a skill, which become chips", () => {
    expect(applyMention("please ask @rev", { start: 11 }, role("reviewer"))).toBe("please ask ");
    expect(applyMention("@", { start: 0 }, skill("review"))).toBe("");
  });

  it("writes a reference to a requirement or a document into the message", () => {
    expect(applyMention("check @log", { start: 6 }, req)).toBe("check Requirement REQ-7 (Login must be rate limited) ");
    expect(applyMention("read @cv", { start: 5 }, doc)).toBe("read /workspace/documents/cv.pdf ");
  });

  it("keeps one role (the last picked) and each skill once", () => {
    let chips = addChip([], role("coder"));
    chips = addChip(chips, skill("review"));
    chips = addChip(chips, skill("review"));
    chips = addChip(chips, role("planner"));
    expect(chips.map((c) => `${c.kind}:${c.id}`)).toEqual(["skill:review", "role:planner"]);
  });
});

describe("MentionPicker", () => {
  const items = [role("coder", "implements a change"), role("reviewer"), skill("review"), req, doc];

  it("groups the items under headings and marks the active one", () => {
    render(<MentionPicker items={items} active={2} onPick={() => undefined} />);
    const list = screen.getByRole("listbox", { name: "Mentions" });
    expect(within(list).getByRole("group", { name: "Roles" })).toBeTruthy();
    expect(within(list).getByRole("group", { name: "Skills" })).toBeTruthy();
    expect(within(list).getByRole("group", { name: "Requirements" })).toBeTruthy();
    expect(within(list).getByRole("group", { name: "Documents" })).toBeTruthy();
    const options = within(list).getAllByRole("option");
    expect(options).toHaveLength(5);
    expect(options.map((o) => o.getAttribute("aria-selected"))).toEqual(["false", "false", "true", "false", "false"]);
    expect(within(list).getByText("@coder")).toBeTruthy();
    expect(within(list).getByText("implements a change")).toBeTruthy();
  });

  it("picks the clicked item and reports the hovered index", () => {
    const onPick = vi.fn();
    const onHover = vi.fn();
    render(<MentionPicker items={items} active={0} onPick={onPick} onHover={onHover} />);
    fireEvent.mouseEnter(screen.getByRole("option", { name: /@reviewer/ }));
    fireEvent.click(screen.getByRole("option", { name: /@reviewer/ }));
    expect(onHover).toHaveBeenCalledWith(1);
    expect(onPick).toHaveBeenCalledWith(items[1]);
  });

  it("renders nothing without items", () => {
    const { container } = render(<MentionPicker items={[]} active={0} onPick={() => undefined} />);
    expect(container.firstChild).toBeNull();
  });
});

describe("MentionChips", () => {
  it("shows what was picked and removes a chip", () => {
    const onRemove = vi.fn();
    render(<MentionChips chips={[role("coder"), skill("review")]} onRemove={onRemove} />);
    expect(screen.getByText("Run as @coder")).toBeTruthy();
    expect(screen.getByText("Skill review")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove coder" }));
    expect(onRemove).toHaveBeenCalledWith(expect.objectContaining({ kind: "role", id: "coder" }));
  });
});
