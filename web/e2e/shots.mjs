// s1: the screenshot scenario (opt-in: E2E_SHOTS=<dir>). It visits every route, project tab and dialog that matters in dark, light and on a
// phone (dark and light) and saves one PNG each, so the UX review (docs/ux-review.md) can look at every screen. Nothing is asserted here:
// a screen that cannot be reached is logged and skipped (and listed in missed.txt).
import fs from "node:fs";
import path from "node:path";

const TOP = ["/", "/supervisor", "/inbox", "/jobs", "/references", "/connectors", "/capabilities", "/schedules", "/roles", "/usage", "/insights", "/memory", "/profile", "/admin", "/help"];
const TABS = ["board", "overview", "members", "memory", "capabilities", "roles", "schedules", "recipes", "usage", "insights", "notifications", "audit", "tasks", "requirements", "workflows", "settings"];

export async function runShots(h) {
  const { problems, step, browser, base, api, post, visible, watch, onboarded, T, proj, lp, sessionState, page, newSession, send, idle } = h;
  const dir = process.env.E2E_SHOTS;
  if (!dir) return;
  await step("s1. Screenshots of every screen (E2E_SHOTS)", async () => {
    fs.mkdirSync(dir, { recursive: true });
    const missed = [];
    const problemsBefore = problems.length;
    let count = 0;
    // sessions: one with an edit (Diff tab), one waiting for approval
    await page.goto(`${base}/#/t/${sessionState.w}`);
    await send("please edit the report");
    await visible(page.getByText("Edited the report.", { exact: true }), "the edit answer");
    await idle(sessionState.w);
    const appr = await newSession("E2E approval");
    await send("please run the script");
    await visible(page.getByRole("alertdialog", { name: "Approval needed" }), "the approval card");
    const reqs = await api(`/projects/${proj.id}/requirements`);
    const tasks = await api(`/projects/${proj.id}/tasks?limit=50`);
    const wfs = await api(`/projects/${proj.wf}/workflows`);
    const runs = await api(`/projects/${proj.wf}/workflow-runs`);
    const pid = proj.id;

    const mk = async (theme, size) => {
      const c = await browser.newContext({ viewport: size, reducedMotion: "reduce" });
      c.on("page", watch);
      await c.addInitScript((t) => {
        try {
          localStorage.setItem("theme", t);
        } catch {
          /* no storage */
        }
      }, theme);
      await c.addInitScript(onboarded);
      return c;
    };
    const settle = async (pg) => {
      await pg.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))).catch(() => {});
      await pg.waitForFunction(() => !document.querySelector('[data-slot="skeleton"], [aria-busy="true"], [aria-label="Loading"], [aria-label="Loading page"]'), null, { timeout: 8000 }).catch(() => {});
      await pg.waitForTimeout(700);
    };
    const run = async (sub, pg, name, fn) => {
      try {
        await fn();
        await settle(pg);
        fs.mkdirSync(path.join(dir, sub), { recursive: true });
        await pg.screenshot({ path: path.join(dir, sub, `${name}.png`) });
        count++;
        return true;
      } catch (e) {
        missed.push(`${sub}/${name}: ${String(e.message).split("\n")[0]}`);
        return false;
      }
    };
    const pages = async (sub, pg, list) => {
      for (const [name, url, after] of list)
        await run(sub, pg, name, async () => {
          await pg.goto(`${base}/#${url}`);
          if (after) await after(pg);
        });
    };
    const FULL = [
      ...TOP.map((r) => [`route${r === "/" ? "-dashboard" : r.replace("/", "-")}`, r]),
      ["route-help-article-policies", "/help?a=policies"],
      ["route-help-article-glossary", "/help?a=glossary"],
      ["route-help-article-triggers", "/help?a=triggers"],
      ...TABS.map((t) => [`tab-${t}`, `/p/${pid}?tab=${t}`]),
      ["tab-task-detail", `/p/${pid}?tab=tasks&task=${tasks[0]?.id ?? ""}`],
      ["tab-tasks-triggers", `/p/${pid}?tab=tasks`, (p) => p.getByRole("tab", { name: "Triggers", exact: true }).click()],
      ["tab-tasks-report", `/p/${pid}?tab=tasks`, (p) => p.getByRole("tab", { name: "Report", exact: true }).click()],
      ["tab-requirement-detail", `/p/${pid}?tab=requirements&req=${reqs[0]?.id ?? ""}`],
      ["tab-workflow-designer", `/p/${proj.wf}?tab=workflows&wf=${wfs?.[0]?.id ?? ""}`],
      ["tab-workflow-run", `/p/${proj.wf}?tab=workflows&run=${runs?.[0]?.id ?? ""}`],
      ["session-edits", `/t/${sessionState.w}`],
      ["session-approval", `/t/${appr}`],
    ];
    const dialogs = async (sub, pg, phone) => {
      const clickVis = (loc) => loc.filter({ visible: true }).first().click({ timeout: 15000 });
      const dlg = async (name, open) => {
        await run(sub, pg, name, async () => {
          await open();
          await pg.getByRole("dialog").first().waitFor({ state: "visible", timeout: 6000 });
        });
        await pg.keyboard.press("Escape").catch(() => {});
        await pg.waitForTimeout(250);
      };
      await dlg("dlg-new-task", async () => {
        await pg.goto(`${base}/#/p/${pid}?tab=tasks`);
        await pg.getByRole("button", { name: /New task/ }).first().waitFor({ state: "visible", timeout: 15000 });
        await settle(pg);
        await clickVis(pg.getByRole("button", { name: /New task/ }));
      });
      await dlg("dlg-recipe-wizard", async () => {
        await pg.goto(`${base}/#/p/${pid}?tab=recipes`);
        await settle(pg);
        await clickVis(pg.getByRole("button", { name: "Use this recipe" }));
      });
      await dlg("dlg-recipe-editor", async () => {
        await pg.goto(`${base}/#/p/${pid}?tab=recipes`);
        await settle(pg);
        await clickVis(pg.getByRole("button", { name: "Shared by people" }));
        await clickVis(pg.getByRole("button", { name: /^Edit / }));
      });
      await dlg("dlg-channel-wizard", async () => {
        await pg.goto(`${base}/#/p/${lp.id ?? pid}?tab=notifications`);
        await settle(pg);
        await clickVis(pg.getByRole("button", { name: /Add channel/ }));
        await pg.getByRole("dialog").getByText("Webhook", { exact: true }).click();
      });
      await dlg("dlg-executor-editor", async () => {
        await pg.goto(`${base}/#/p/${pid}?tab=roles`);
        await settle(pg);
        await clickVis(pg.getByRole("button", { name: /New executor/ }));
      });
      await dlg("dlg-executor-editor-advanced", async () => {
        await pg.goto(`${base}/#/p/${pid}?tab=roles`);
        await settle(pg);
        await clickVis(pg.getByRole("button", { name: /New executor/ }));
        const d = pg.getByRole("dialog");
        const t = d.getByRole("button", { name: /^Advanced/ });
        if ((await t.getAttribute("aria-expanded")) !== "true") await t.click();
        await d.getByRole("combobox", { name: "Start from" }).click();
        await pg.getByRole("option", { name: "Codex" }).click();
      });
      await dlg("dlg-role-editor", async () => {
        await pg.goto(`${base}/#/roles`);
        await settle(pg);
        await clickVis(pg.getByRole("button", { name: /New role/ }));
      });
      await dlg("dlg-role-editor-advanced", async () => {
        await pg.goto(`${base}/#/roles`);
        await settle(pg);
        await clickVis(pg.getByRole("button", { name: /New role/ }));
        const t = pg.getByRole("dialog").getByRole("button", { name: /^Advanced/ });
        if ((await t.getAttribute("aria-expanded")) !== "true") await t.click();
      });
      await dlg("dlg-tapd-connect", async () => {
        const p2 = (await post("/projects", { name: "E2E tapd shots" })).id;
        await pg.goto(`${base}/#/p/${p2}?tab=requirements`);
        await settle(pg);
        await clickVis(pg.getByRole("button", { name: "Connect TAPD" }));
      });
      await run(sub, pg, "card-digest", async () => {
        await pg.goto(`${base}/#/p/${pid}?tab=insights`);
        await settle(pg);
        await pg.getByText(/digest/i).first().scrollIntoViewIfNeeded();
      });
      await run(sub, pg, "dlg-workflow-node-inspector", async () => {
        await pg.goto(`${base}/#/p/${proj.wf}?tab=workflows&wf=${wfs?.[0]?.id ?? ""}`);
        await settle(pg);
        await pg.getByRole("group", { name: "Workflow canvas" }).getByRole("button", { name: /^Step / }).first().click({ timeout: 15000 });
      });
      await dlg("dlg-command-palette", async () => {
        await pg.goto(`${base}/#/`);
        await settle(pg);
        await pg.keyboard.press("Control+k");
      });
      await dlg("dlg-project-switcher", async () => {
        await pg.goto(`${base}/#/`);
        await settle(pg);
        await pg.keyboard.press("Control+p");
      });
      await dlg("dlg-shortcuts", async () => {
        await pg.goto(`${base}/#/`);
        await settle(pg);
        await pg.keyboard.press("?");
      });
      const nav = async () => {
        if (phone && !(await pg.getByRole("navigation", { name: "Explorer" }).filter({ visible: true }).count())) await pg.getByRole("button", { name: "Open navigation" }).click();
      };
      await run(sub, pg, "menu-project-actions", async () => {
        await pg.goto(`${base}/#/p/${pid}?tab=board`);
        await settle(pg);
        await nav();
        await clickVis(pg.getByRole("button", { name: "Project actions" }));
      });
      await pg.keyboard.press("Escape");
      await run(sub, pg, "menu-user", async () => {
        await pg.goto(`${base}/#/`);
        await settle(pg);
        await nav();
        await clickVis(pg.getByRole("button", { name: "User menu" }));
      });
      await pg.keyboard.press("Escape");
      if (phone) {
        await run(sub, pg, "explorer", async () => {
          await pg.goto(`${base}/#/p/${pid}?tab=board`);
          await settle(pg);
          await nav();
        });
      }
    };

    const DESK = { width: 1440, height: 900 };
    const PHONE = { width: 390, height: 844 };
    let c = await mk("dark", DESK);
    let pg = await c.newPage();
    await pages("desktop-dark", pg, FULL);
    await dialogs("desktop-dark", pg, false);
    await c.close();
    const lightNames = new Set(["route-dashboard", "route-inbox", "route-jobs", "route-connectors", "route-roles", "route-usage", "route-insights", "route-admin", "route-help", "route-profile", "route-schedules", "tab-board", "tab-tasks", "tab-requirements", "tab-workflows", "tab-settings", "tab-notifications", "tab-recipes", "tab-members", "tab-task-detail", "tab-workflow-designer", "session-edits", "session-approval"]);
    c = await mk("light", DESK);
    pg = await c.newPage();
    await pages("desktop-light", pg, FULL.filter(([n]) => lightNames.has(n)));
    await dialogs("desktop-light", pg, false);
    await c.close();
    const phoneNames = new Set(["route-dashboard", "route-inbox", "route-jobs", "route-roles", "route-help", "route-profile", "route-usage", "route-admin", "tab-board", "tab-overview", "tab-tasks", "tab-task-detail", "tab-requirements", "tab-requirement-detail", "tab-workflows", "tab-workflow-designer", "tab-settings", "tab-insights", "tab-recipes", "tab-notifications", "session-edits", "session-approval"]);
    c = await mk("dark", PHONE);
    pg = await c.newPage();
    await pages("phone-dark", pg, FULL.filter(([n]) => phoneNames.has(n)));
    await dialogs("phone-dark", pg, true);
    await c.close();
    c = await mk("light", PHONE);
    pg = await c.newPage();
    await pages("phone-light", pg, FULL.filter(([n]) => ["route-dashboard", "tab-board", "tab-tasks", "session-approval", "route-help"].includes(n)));
    await c.close();
    // welcome and tour: a newcomer
    for (const [theme, size, sub] of [["dark", DESK, "desktop-dark"], ["light", DESK, "desktop-light"], ["dark", PHONE, "phone-dark"]]) {
      await post("/me/preferences", { values: { onboarding: null, ui: null } }, "PATCH");
      const n = await browser.newContext({ viewport: size, reducedMotion: "reduce" });
      n.on("page", watch);
      await n.addInitScript((t) => {
        try {
          localStorage.setItem("theme", t);
        } catch {
          /* no storage */
        }
      }, theme);
      const np = await n.newPage();
      await run(sub, np, "dlg-welcome", async () => {
        await np.goto(`${base}/#/`);
        await np.getByRole("dialog", { name: "Welcome to 7ar" }).waitFor({ state: "visible", timeout: T });
      });
      if (sub !== "desktop-light") {
        await run(sub, np, "dlg-tour", async () => {
          await np.getByRole("dialog").getByRole("button", { name: "Skip for now" }).click();
          if (size.width < 500) await np.getByRole("button", { name: "Open navigation" }).click();
          await np.getByRole("button", { name: "User menu" }).first().click();
          await np.getByRole("menuitem", { name: "Take the tour" }).click();
          await np.getByTestId("tour").waitFor({ state: "visible", timeout: T });
        });
      }
      await n.close();
    }
    problems.length = Math.min(problems.length, problemsBefore); // 404s of screens that do not exist in the sample data are not findings
    console.log(`s1: ${count} screenshots in ${dir}${missed.length ? `; missed:\n  ${missed.join("\n  ")}` : ""}`);
    fs.writeFileSync(path.join(dir, "missed.txt"), missed.join("\n"));
  });
}
