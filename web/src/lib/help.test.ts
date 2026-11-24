import { describe, expect, it } from "vitest";
import { ARTICLES, GROUPS, articleBySlug, helpHref, searchArticles } from "./help";

describe("help articles", () => {
  it("have unique slugs, known groups and a non-empty body", () => {
    expect(new Set(ARTICLES.map((a) => a.slug)).size).toBe(ARTICLES.length);
    for (const a of ARTICLES) {
      expect(GROUPS).toContain(a.group);
      expect(a.body.length).toBeGreaterThan(40);
    }
  });

  it("only link to articles that exist", () => {
    for (const a of ARTICLES) for (const m of a.body.matchAll(/\(help:([a-z-]+)\)/g)) expect(articleBySlug(m[1]), `${a.slug} -> ${m[1]}`).toBeTruthy();
  });

  it("cover the articles the contextual help buttons open", () => {
    for (const s of ["policies", "budgets", "executors", "gates", "triggers", "approvals", "hand-off-a-task", "shortcuts", "faq"]) expect(articleBySlug(s)).toBeTruthy();
  });

  it("search matches every word and ranks titles first", () => {
    expect(searchArticles("").length).toBe(ARTICLES.length);
    expect(searchArticles("supervised")[0]?.slug).toBe("policies");
    expect(searchArticles("budget cap")[0]?.slug).toBe("budgets");
    expect(searchArticles("zzzzqq")).toEqual([]);
  });

  it("builds deep links", () => {
    expect(helpHref("policies")).toBe("/help?a=policies");
    expect(helpHref()).toBe("/help");
  });
});
