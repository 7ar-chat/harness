// p: the UX and accessibility audit. p1 runs axe on every route, tab and dialog that matters; p2 walks through the product as a newcomer.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { axeScan, describe } from "./axe.mjs";

const TOP = ["/", "/supervisor", "/inbox", "/jobs", "/references", "/connectors", "/capabilities", "/schedules", "/roles", "/usage", "/insights", "/memory", "/profile", "/admin", "/help"];
// the project tabs of the h5 route sweep
export const TABS = ["board", "overview", "members", "memory", "capabilities", "roles", "schedules", "recipes", "usage", "insights", "notifications", "audit", "tasks", "requirements", "workflows", "settings"];

export async function runP(h) {
  const { step, browser, base, post, visible, gone, poll, eq, watch, onboarded, T, proj, hookUrl } = h;
  const settle = async (pg) => {
    await pg.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
    await pg.waitForFunction(() => !document.querySelector('[data-slot="skeleton"], [aria-busy="true"], [aria-label="Loading"], [aria-label="Loading page"]'), null, { timeout: T });
    await pg.waitForTimeout(350); // fade-in transitions finish, so contrast is measured on settled colours
  };
  // The one e2e user keeps its onboarding state on the server now (the account follows the person): a browser that is meant
  // to be a newcomer must start from an empty server-side document, whatever earlier scenarios dismissed.
  const forgetOnboarding = () => post("/me/preferences", { values: { onboarding: null, ui: null } }, "PATCH");
  const mk = async (theme, size, { seen = true } = {}) => {
    if (!seen) await forgetOnboarding();
    const c = await browser.newContext({ viewport: size, bypassCSP: true, reducedMotion: "reduce" });
    c.on("page", watch);
    await c.addInitScript((t) => {
      try {
        localStorage.setItem("theme", t);
      } catch {
        /* no storage */
      }
    }, theme);
    if (seen) await c.addInitScript(onboarded);
    return c;
  };
  const DESK = { width: 1440, height: 900 };
  const PHONE = { width: 390, height: 844 };
  const configs = [
    ["dark", DESK, "desktop dark"],
    ["light", DESK, "desktop light"],
    ["dark", PHONE, "phone dark"],
    ["light", PHONE, "phone light"],
  ];
  const found = []; // every violation of the run (also the ones of pages that pass), for the counts
  const tally = (label, vs) => {
    for (const v of vs) found.push({ label, id: v.id, nodes: v.nodes.length });
  };

  await step("p1. Accessibility: axe finds no serious or critical violation on any route, project tab or key dialog, in both themes and on a phone", async () => {
    const pid = proj?.id ?? (await post("/projects", { name: "E2E axe" })).id;
    const failures = [];
    const scan = async (pg, label) => {
      const vs = await axeScan(pg);
      tally(label, vs);
      failures.push(...describe(label, vs));
    };
    for (const [theme, size, name] of configs) {
      const c = await mk(theme, size);
      const pg = await c.newPage();
      try {
        for (const r of TOP) {
          await pg.goto(`${base}/#${r}`);
          await settle(pg);
          await scan(pg, `${name} ${r}`);
        }
        for (const t of TABS) {
          await pg.goto(`${base}/#/p/${pid}?tab=${t}`);
          await pg.getByRole("tab", { selected: true }).first().waitFor({ state: "visible", timeout: T });
          await settle(pg);
          await scan(pg, `${name} tab ${t}`);
        }
        // dialogs
        const dlgScan = async (what) => {
          await pg.getByRole("dialog").first().waitFor({ state: "visible", timeout: T });
          await pg.waitForTimeout(400);
          await scan(pg, `${name} dialog ${what}`);
          await pg.keyboard.press("Escape");
          await gone(pg.getByRole("dialog"), `${what} closed`).catch(() => {});
        };
        await pg.goto(`${base}/#/p/${pid}?tab=tasks`);
        await settle(pg);
        await pg.getByRole("button", { name: /New task/ }).filter({ visible: true }).first().click();
        await dlgScan("new task");
        await pg.goto(`${base}/#/p/${pid}?tab=recipes`);
        await settle(pg);
        await pg.getByRole("button", { name: "Use this recipe" }).first().click();
        await dlgScan("recipe wizard");
        await pg.goto(`${base}/#/p/${pid}?tab=notifications`);
        await settle(pg);
        await pg.getByRole("button", { name: /Add channel/ }).filter({ visible: true }).first().click();
        await pg.getByRole("dialog").getByText("Webhook", { exact: true }).click();
        await dlgScan("channel wizard");
        await pg.goto(`${base}/#/`);
        await settle(pg);
        await pg.keyboard.press("Control+k");
        await pg.getByRole("option").first().waitFor({ state: "visible", timeout: T });
        await dlgScan("command palette");
        await pg.keyboard.press("?");
        await dlgScan("shortcuts");
      } finally {
        await c.close();
      }
      // welcome and tour are for a person who has not seen them
      const n = await mk(theme, size, { seen: false });
      const np = await n.newPage();
      try {
        await np.goto(`${base}/#/`);
        await np.getByRole("dialog", { name: "Welcome to 7ar" }).waitFor({ state: "visible", timeout: T });
        await np.waitForTimeout(400);
        await scan(np, `${name} dialog welcome`);
        await np.getByRole("dialog").getByRole("button", { name: "Skip for now" }).click();
        await gone(np.getByRole("dialog"), "welcome closed");
        if (size.width < 500) await np.getByRole("button", { name: "Open navigation" }).click();
        await np.getByRole("button", { name: "User menu" }).first().click();
        await np.getByRole("menuitem", { name: "Take the tour" }).click();
        await np.getByTestId("tour").waitFor({ state: "visible", timeout: T });
        await np.waitForTimeout(400);
        await scan(np, `${name} dialog tour`);
      } finally {
        await n.close();
      }
    }
    // counts for the report: nodes per rule over the whole run
    const byRule = {};
    for (const f of found) byRule[f.id] = (byRule[f.id] ?? 0) + f.nodes;
    console.log(`p1 axe: ${found.length} failing page/dialog scans, ${Object.values(byRule).reduce((a, b) => a + b, 0)} failing nodes ${JSON.stringify(byRule)}`);
    fs.mkdirSync(path.join(os.tmpdir(), "harness-e2e-failures"), { recursive: true });
    fs.writeFileSync(path.join(os.tmpdir(), "harness-e2e-failures", "p1-axe.txt"), failures.join("\n"));
    if (failures.length) throw new Error(`${failures.length} axe findings\n${failures.slice(0, 60).join("\n")}`);
  });

  // ---- p2: a newcomer's first minutes ---------------------------------------------------------------------------------------------------
  const TERMS = ["Policy", "Budget", "Executor", "Workflow", "Gate", "Trigger"];
  const walk = async (size, label) => {
    await forgetOnboarding();
    const c = await browser.newContext({ viewport: size, bypassCSP: true, reducedMotion: "reduce" }); // a new person: nothing is marked as seen
    c.on("page", watch);
    await c.addInitScript(() => {
      window.__cls = 0; // layout shifts the person did not cause
      window.__clsLog = [];
      try {
        new PerformanceObserver((l) => {
          for (const e of l.getEntries())
            if (!e.hadRecentInput) {
              window.__cls += e.value;
              window.__clsLog.push(`${e.value.toFixed(3)} ${(e.sources ?? []).map((x) => `${x.node?.nodeName}.${String(x.node?.className ?? "").slice(0, 50)}`).join(" | ")}`);
            }
        }).observe({ type: "layout-shift", buffered: true });
      } catch {
        /* no layout-shift entries in this browser */
      }
    });
    const pg = await c.newPage();
    let clicks = 0;
    const click = async (loc, what) => {
      await visible(loc, what);
      clicks++;
      await loc.filter({ visible: true }).first().click();
    };
    const problems = [];
    // the next action is on screen without scrolling, the page does not scroll sideways, and nothing jumped since the last check
    const check = async (step, next, what) => {
      const el = next.filter({ visible: true }).first();
      await el.waitFor({ state: "visible", timeout: T }).catch(() => problems.push(`${label} ${step}: ${what} is not visible`));
      const box = await el.boundingBox().catch(() => null);
      const vp = pg.viewportSize();
      if (!box) problems.push(`${label} ${step}: ${what} has no box`);
      else if (box.y < 0 || box.y + box.height > vp.height + 1 || box.x < 0 || box.x + box.width > vp.width + 1) problems.push(`${label} ${step}: ${what} is outside the first screen (${Math.round(box.y)}..${Math.round(box.y + box.height)} of ${vp.height})`);
      const over = await pg.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      if (over > 0) problems.push(`${label} ${step}: the page scrolls sideways by ${over}px`);
      const { cls, log } = await pg.evaluate(() => {
        const v = { cls: window.__cls, log: window.__clsLog.splice(0) };
        window.__cls = 0;
        return v;
      });
      // (an approval card arriving in a live chat grows the conversation on purpose: only page loads are held to the limit)
      if (cls > 0.1 && !/^approval/.test(step)) problems.push(`${label} ${step}: layout shift ${cls.toFixed(3)} ${log.join(" ; ")}`);
    };
    const nav = async () => {
      if (size.width < 500 && !(await pg.getByRole("navigation", { name: "Explorer" }).filter({ visible: true }).count())) {
        await pg.getByRole("button", { name: "Open navigation" }).click();
        await pg.getByRole("navigation", { name: "Explorer" }).waitFor({ state: "visible", timeout: T });
      }
    };
    try {
      // 1. welcome -> the sample, one click
      await pg.goto(`${base}/#/`);
      const dlg = pg.getByRole("dialog", { name: "Welcome to 7ar" });
      await visible(dlg, "the welcome");
      await check("welcome", dlg.getByRole("button", { name: "Try a sample" }), "the Try a sample button");
      await click(dlg.getByRole("button", { name: "Try a sample" }), "Try a sample");
      await pg.waitForURL(/#\/t\/[^/]+$/, { timeout: T });
      // 2. the agent works: the first answer is on screen
      await visible(pg.getByText("Hello from the fake model."), "the first agent output");
      const toFirst = clicks;
      if (toFirst > 4) problems.push(`${label}: ${toFirst} clicks from the first screen to the first agent output (limit 4)`);
      console.log(`p2 ${label}: ${toFirst} click(s) from the welcome to the first agent output`);
      const composer = pg.getByRole("textbox", { name: "Message" });
      await check("first output", composer, "the message box");
      // 3. an action that needs approval, approved
      await composer.fill("please run the script");
      await pg.getByRole("button", { name: "Send", exact: true }).click();
      const card = pg.getByRole("alertdialog", { name: "Approval needed" });
      await check("approval", card.getByRole("button", { name: "Approve", exact: true }), "the Approve button");
      await click(card.getByRole("button", { name: "Approve", exact: true }), "Approve");
      await visible(pg.getByText("The script finished."), "the answer after approval");
      // 4. Needs you: a second approval waits, and is found from the list
      await composer.fill("please run the script");
      await pg.getByRole("button", { name: "Send", exact: true }).click();
      await visible(card, "the second approval");
      await pg.evaluate(() => {
        window.__cls = 0; // the card arriving in the live chat is not a page load
        window.__clsLog.length = 0;
      });
      await nav();
      await click(pg.getByRole("navigation", { name: "Explorer" }).getByRole("link", { name: /^Needs you/ }), "Needs you");
      await check("needs you", pg.getByRole("main").getByRole("heading", { name: "Needs you" }), "the Needs you page");
      const item = pg.getByRole("main").getByRole("link", { name: /Try 7ar/ });
      // (the list holds everything that waits across all projects, so the item may sit below the first screen of a busy account)
      await visible(item, "the waiting session in the list");
      await item.first().scrollIntoViewIfNeeded();
      await click(item, "the waiting session");
      await check("approval 2", card.getByRole("button", { name: "Approve", exact: true }), "the Approve button");
      await click(card.getByRole("button", { name: "Approve", exact: true }), "Approve");
      await gone(card, "approval card gone");
      // 5. a task from the Tasks tab
      await nav();
      const explorer = pg.getByRole("navigation", { name: "Explorer" });
      await click(explorer.getByRole("link", { name: "Work", exact: true }), "Work");
      await click(pg.getByRole("main").getByRole("tab", { name: "Tasks", exact: true }), "the Tasks view");
      const first = pg.getByRole("main").getByRole("button", { name: /Create your first task|New task/ });
      await check("tasks", first, "the create task button");
      await click(first, "New task");
      const form = pg.getByRole("dialog");
      await form.getByLabel("Title").fill("Say hello");
      await form.getByLabel("Spec").fill("Greet the team in one sentence.");
      await check("new task", form.getByRole("button", { name: "Create task" }), "the Create task button");
      // jargon in this form has a ? that opens its article, and the article links to the glossary
      for (const name of ["Policy (help)", "Executors (help)"]) {
        if (!(await form.getByRole("button", { name }).count())) problems.push(`${label} new task: no "${name}" next to the term`);
      }
      const adv = form.getByRole("button", { name: /Advanced options/ });
      if (await adv.count()) await adv.click();
      if (!(await form.getByRole("button", { name: "Budget (help)" }).count())) problems.push(`${label} new task: no "Budget (help)" next to the budget`);
      await click(form.getByRole("button", { name: "Create task" }), "Create task");
      await gone(pg.getByRole("dialog"), "the task form closed");
      // 6. a recipe
      await nav();
      await click(explorer.getByRole("link", { name: "Automation", exact: true }), "Automation");
      await click(pg.getByRole("main").getByRole("tab", { name: "Recipes", exact: true }), "the Recipes view");
      const search = pg.getByLabel("Search recipes");
      await check("recipes", search, "the recipe search");
      await search.fill("digest");
      await check("recipe found", pg.getByRole("list", { name: "Recipes" }).getByRole("button", { name: "Use this recipe" }), "Use this recipe");
      // 7. Help, and the glossary that explains the jargon
      await click(pg.getByRole("button", { name: "Help", exact: true }), "Help");
      const find = pg.getByRole("main").getByRole("searchbox", { name: "Search the help" });
      await check("help", find, "the help search");
      await find.fill("glossary");
      const hit = pg.getByRole("main").getByRole("list", { name: "Search results" }).getByRole("button", { name: /Glossary/ });
      await check("help search", hit, "the glossary article");
      await click(hit, "Glossary");
      for (const t of TERMS) {
        if (!(await pg.getByRole("main").getByRole("cell", { name: t, exact: true }).count())) problems.push(`${label} glossary: "${t}" is not explained`);
      }
      // the places that use the jargon link to it: each article of the six ends with the glossary link
      for (const slug of ["policies", "budgets", "executors", "workflows", "gates", "triggers"]) {
        await pg.goto(`${base}/#/help?a=${slug}`);
        await visible(pg.getByRole("main").getByRole("heading", { level: 2 }), `the ${slug} article`);
        if (!(await pg.getByRole("main").getByRole("button", { name: "glossary" }).filter({ visible: true }).count())) problems.push(`${label} help ${slug}: no link to the glossary`);
      }
    } finally {
      await c.close();
    }
    return problems;
  };

  await step("p2. Newcomer walkthrough: welcome, sample, approval, Needs you, task, recipe and help, each next action on the first screen, at most 4 clicks to the first agent output", async () => {
    const all = [...(await walk({ width: 1440, height: 900 }, "desktop")), ...(await walk({ width: 390, height: 844 }, "phone"))];
    if (all.length) throw new Error(all.join("\n"));
  });

  // t. The short navigation: four project areas and Me, views as sub-navigation, job search as a use case
  await step("t1. Navigation: the explorer lists the short set; Work and its views change the tab id in the URL; Job search is a use case on the Dashboard and in the palette, not in the explorer", async () => {
    const pid = proj?.id ?? (await post("/projects", { name: "E2E nav" })).id;
    const c = await mk("dark", DESK);
    const pg = await c.newPage();
    try {
      await pg.goto(`${base}/#/p/${pid}`);
      const explorer = pg.getByRole("navigation", { name: "Explorer" });
      await visible(explorer.getByRole("link", { name: "Work", exact: true }), "the Work area");
      const short = ["Dashboard", "Needs you", "Work", "Automation", "Insights", "Settings", "My assistant", "Me"];
      const names = (await explorer.getByRole("link").allInnerTexts()).map((t) => t.replace(/\s+/g, " ").trim().replace(/^Needs you \d+$/, "Needs you")); // the count badge is part of the link's text
      eq(names.filter((n) => short.includes(n)), short, "the explorer's places, in order");
      for (const old of [/^Jobs?\b/i, /^Job search/i, /^References/, /^Connectors/, /^Members/, /^Usage/, /^Audit/, /^Recipes/, /^Schedules/, /^Roles/, /^Insights \S/, /^My (memory|schedules|roles|tools)/]) {
        const left = names.filter((n) => old.test(n));
        eq(left, [], `no explorer link like ${old}`);
      }
      eq(await pg.getByRole("link", { name: "Settings", exact: true }).count(), 2, "the project's Settings and the account's Settings");
      // Work, then its views: the tab id follows
      await explorer.getByRole("link", { name: "Work", exact: true }).click();
      const main = pg.getByRole("main");
      await visible(main.getByRole("tablist", { name: "Work views" }), "the Work views");
      const tabId = () => new URL(pg.url().replace("#/", "")).searchParams.get("tab");
      eq(tabId(), "board", "Work opens on the board");
      await main.getByRole("tab", { name: "Tasks", exact: true }).click();
      await poll(async () => tabId() === "tasks", "tab=tasks in the URL");
      await main.getByRole("tab", { name: "Requirements", exact: true }).click();
      await poll(async () => tabId() === "requirements", "tab=requirements in the URL");
      await visible(pg.getByRole("tablist", { name: "Open editors" }).getByRole("tab", { name: "Work · Requirements", selected: true }), "one editor tab for the area, titled with the view");
      // the area remembers its view; an old deep link still lands in the right area
      await explorer.getByRole("link", { name: "Insights", exact: true }).click();
      await main.getByRole("tab", { name: "Usage & caps", exact: true }).focus();
      await pg.keyboard.press("ArrowRight");
      await poll(async () => tabId() === "audit", "ArrowRight moved to Activity (tab=audit)");
      await pg.goto(`${base}/#/p/${pid}?tab=members`);
      await visible(main.getByRole("tab", { name: "Members & access", selected: true }), "the Members view in Settings");
      eq(await explorer.getByRole("link", { name: "Settings", exact: true }).first().getAttribute("aria-current"), "page", "the area is highlighted");
      // Me
      await pg.goto(`${base}/#/references`);
      await visible(main.getByRole("tablist", { name: "Me views" }), "the Me views");
      // Job search: a use case
      await pg.goto(`${base}/#/`);
      const cases = pg.getByRole("region", { name: "Use cases" });
      await visible(cases.getByRole("link", { name: /Job search assistant/ }), "the Job search card on the Dashboard");
      await pg.keyboard.press("Control+k");
      await pg.getByPlaceholder(/Jump to a session/).fill("use case");
      const entry = pg.getByRole("option", { name: "Use case: Job search" });
      await visible(entry, "the palette entry");
      await entry.click();
      await poll(async () => /#\/jobs$/.test(pg.url()), "the job search page opened");
      await visible(main.getByRole("heading", { name: "Job search" }), "the Job search page");
    } finally {
      await c.close();
    }
  });

  // t2 (opt-in, E2E_SHOTS=<dir>): the navigation screens, saved to look at; nothing is asserted.
  await step("t2. Navigation screenshots (E2E_SHOTS)", async () => {
    const dir = process.env.E2E_SHOTS;
    if (!dir) return;
    fs.mkdirSync(dir, { recursive: true });
    const pid = proj?.id ?? (await post("/projects", { name: "E2E nav" })).id;
    for (const [theme, size, name] of configs) {
      const phone = size.width < 500;
      const c = await mk(theme, size);
      const pg = await c.newPage();
      const shot = async (what) => {
        await settle(pg).catch(() => {});
        await pg.screenshot({ path: path.join(dir, `nav-${name.replace(" ", "-")}-${what}.png`) });
      };
      try {
        const at = async (hash, ready) => {
          await pg.goto(`${base}/#${hash}`);
          await ready.waitFor({ state: "visible", timeout: T });
        };
        const views = (n) => pg.getByRole("tablist", { name: n });
        await at(`/p/${pid}?tab=board`, views("Work views"));
        if (phone) {
          await pg.getByRole("button", { name: /Toggle explorer/ }).click().catch(() => {});
          await shot("explorer-drawer");
          await pg.keyboard.press("Escape");
        } else {
          await shot("work-board-explorer-open");
        }
        for (const [hash, list, what] of [
          [`/p/${pid}?tab=requirements`, "Work views", "work-requirements"],
          [`/p/${pid}?tab=tasks`, "Work views", "work-tasks"],
          [`/p/${pid}?tab=workflows`, "Automation views", "automation"],
          [`/p/${pid}?tab=insights`, "Insights views", "insights"],
          [`/p/${pid}?tab=members`, "Settings views", "settings-members"],
          [`/references`, "Me views", "me"],
        ]) {
          await at(hash, views(list));
          await shot(what);
        }
        if (!phone) {
          await at(`/p/${pid}?tab=board`, views("Work views"));
          await pg.keyboard.press("Control+b");
          await pg.waitForTimeout(400);
          await shot("work-board-explorer-collapsed");
        }
        await pg.goto(`${base}/#/`);
        await pg.getByRole("heading", { name: "Dashboard" }).waitFor({ state: "visible", timeout: T });
        await shot("dashboard");
      } finally {
        await c.close();
      }
    }
  });
}
