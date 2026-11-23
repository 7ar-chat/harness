import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Workflow, WorkflowNode, WorkflowRun, WorkflowRunDetail } from "@/api/types";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const T = "2026-01-01T00:00:00Z";
const node = (id: string, extra: Partial<WorkflowNode> = {}): WorkflowNode => ({ id, kind: "agent", title: id, prompt: `do ${id}`, role: "coder", pos: { x: 40, y: 40 }, ...extra });
const wf = (extra: Partial<Workflow> = {}): Workflow => ({
  id: "w1", projectId: "p1", name: "Delivery", description: "plan then build", status: "draft", version: 3, publishedVersion: 0, createdAt: T, updatedAt: T, updatedBy: "alice",
  inputs: [{ name: "task", required: true }],
  nodes: [node("plan", { role: "planner" }), node("review", { kind: "gate", prompt: undefined, role: undefined, approvers: ["role:member"], dependsOn: ["plan"], pos: { x: 300, y: 40 } })],
  ...extra,
});
const template = { id: "bug-fix", name: "Bug fix", description: "Reproduce, fix, test.", workflow: wf({ name: "Bug fix", nodes: [node("reproduce"), node("fix", { dependsOn: ["reproduce"] })] }) };

const routes = (r: "owner" | "member" | "viewer", extra: Record<string, unknown> = {}) => {
  const p = project("p1", { name: "payments", role: r });
  return {
    ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/projects/p1/members": [],
    "GET /api/projects/p1/github": reply(404),
    "GET /api/memory/stats": reply(404),
    "GET /api/projects/p1/workflows": [wf(), wf({ id: "w2", name: "Published one", status: "published", publishedVersion: 2, version: 2 })],
    "GET /api/workflow-templates": [template],
    "GET /api/projects/p1/requirements": [{ id: "r1", title: "Faster login" }],
    ...extra,
  };
};
const page = () => within(screen.getByRole("main"));
const dialog = () => within(screen.getByRole("dialog"));

describe("the Workflows tab", () => {
  it("lists workflows and the template gallery, and copies a template", async () => {
    const f = mockFetch(routes("member", { "POST /api/projects/p1/workflows/from-template": wf({ id: "w1", name: "Bug fix" }) }));
    await renderApp("/p/p1?tab=workflows");

    expect(await page().findByText("Delivery")).toBeTruthy();
    expect(page().getByText("Published one")).toBeTruthy();
    expect(page().getByText("published v2")).toBeTruthy();
    expect(await page().findByText("reproduce → fix")).toBeTruthy();
    fireEvent.click(page().getByRole("button", { name: "Use the Bug fix template" }));

    await waitFor(() => expect(f.find("POST", "/api/projects/p1/workflows/from-template")).toHaveLength(1));
    expect(f.find("POST", "/api/projects/p1/workflows/from-template")[0]?.body).toEqual({ template: "bug-fix" });
    // it opens in the designer
    expect(await page().findByLabelText("Workflow name")).toBeTruthy();
  });

  it("only a published workflow can run, and the run dialog asks for its inputs", async () => {
    const f = mockFetch(routes("member", { "POST /api/projects/p1/workflow-runs": { id: "run1" } }));
    await renderApp("/p/p1?tab=workflows");
    await page().findByText("Delivery");

    const runButtons = page().getAllByRole("button", { name: /^Run$/ });
    expect((runButtons[0] as HTMLButtonElement).disabled).toBe(true); // the draft
    fireEvent.click(runButtons[1]!);
    const start = await dialog().findByRole("button", { name: /Start the run/ });
    expect((start as HTMLButtonElement).disabled).toBe(true); // a required input is empty
    fireEvent.change(dialog().getByLabelText(/task/), { target: { value: "ship login" } });
    fireEvent.change(dialog().getByLabelText("Requirement (optional)"), { target: { value: "r1" } });
    fireEvent.click(start);

    await waitFor(() => expect(f.find("POST", "/api/projects/p1/workflow-runs")).toHaveLength(1));
    expect(f.find("POST", "/api/projects/p1/workflow-runs")[0]?.body).toEqual({ workflowId: "w2", inputs: { task: "ship login" }, requirementId: "r1" });
  });

  it("a viewer can look but not change or run", async () => {
    mockFetch(routes("viewer"));
    await renderApp("/p/p1?tab=workflows");
    await page().findByText("Delivery");
    expect(page().queryByRole("button", { name: /New workflow/ })).toBeNull();
    expect(page().queryByRole("button", { name: /^Run$/ })).toBeNull();
    expect(page().queryByRole("button", { name: /Use the Bug fix template/ })).toBeNull();
    expect(page().getAllByRole("button", { name: "View" })).toHaveLength(2);
  });
});

describe("the designer", () => {
  const open = async () => {
    const r = await renderApp("/p/p1?tab=workflows&wf=w1");
    await screen.findByRole("group", { name: "Workflow canvas" });
    return r;
  };
  const canvas = () => within(screen.getByRole("group", { name: "Workflow canvas" }));

  it("draws the steps and edits one in the inspector", async () => {
    const f = mockFetch(routes("member", { "PUT /api/projects/p1/workflows/w1": wf({ version: 4 }) }));
    await open();
    expect(await canvas().findByRole("button", { name: "Step plan" })).toBeTruthy();
    expect(canvas().getByRole("button", { name: "Step review" })).toBeTruthy();
    expect(canvas().getByRole("button", { name: "Connection from plan to review" })).toBeTruthy();

    fireEvent.click(canvas().getByRole("button", { name: "Step plan" }));
    fireEvent.change(await page().findByLabelText("Prompt"), { target: { value: "plan {{inputs.task}} carefully" } });
    fireEvent.change(page().getByLabelText("Role"), { target: { value: "explorer" } });
    fireEvent.change(page().getByLabelText("Model"), { target: { value: "cheap" } });
    fireEvent.change(page().getByLabelText("Deliverables"), { target: { value: "plan, risks" } });
    fireEvent.click(page().getByRole("button", { name: "Save" }));

    await waitFor(() => expect(f.find("PUT", "/api/projects/p1/workflows/w1")).toHaveLength(1));
    const sent = f.find("PUT", "/api/projects/p1/workflows/w1")[0]?.body as { version: number; nodes: WorkflowNode[]; name: string };
    expect(sent.version).toBe(3);
    expect(sent.name).toBe("Delivery");
    expect(sent.nodes[0]).toMatchObject({ id: "plan", role: "explorer", model: "cheap", prompt: "plan {{inputs.task}} carefully", outputs: ["plan", "risks"] });
  });

  it("adds steps after the selected one and connects two steps by clicking", async () => {
    mockFetch(routes("member"));
    await open();
    fireEvent.click(await canvas().findByRole("button", { name: "Step plan" }));
    fireEvent.click(page().getByRole("button", { name: "Script" })); // added after "plan"
    expect(await canvas().findByRole("button", { name: "Step Script" })).toBeTruthy();
    expect(canvas().getByRole("button", { name: "Connection from plan to script" })).toBeTruthy();

    // connect review -> script: click the dot of review, then the script box
    fireEvent.click(canvas().getByRole("button", { name: "Connect from review" }));
    expect(page().getByText(/Click the step that should wait for review/)).toBeTruthy();
    fireEvent.click(canvas().getByRole("button", { name: "Step Script" }));
    expect(await canvas().findByRole("button", { name: "Connection from review to script" })).toBeTruthy();

    // a loop is refused
    fireEvent.click(canvas().getByRole("button", { name: "Connect from script" }));
    fireEvent.click(canvas().getByRole("button", { name: "Step plan" }));
    expect(canvas().queryByRole("button", { name: "Connection from script to plan" })).toBeNull();
  });

  it("removes a connection from the inspector", async () => {
    mockFetch(routes("member"));
    await open();
    fireEvent.click(await canvas().findByRole("button", { name: "Step review" }));
    fireEvent.click(await page().findByRole("button", { name: "Stop waiting for plan" }));
    await waitFor(() => expect(canvas().queryByRole("button", { name: "Connection from plan to review" })).toBeNull());
  });

  it("shows what is wrong while editing and after the server refuses", async () => {
    const f = mockFetch(
      routes("member", {
        "PUT /api/projects/p1/workflows/w1": reply(400, { error: { code: "invalid_workflow", message: "plan: the role \"wizard\" does not exist", issues: [{ nodeId: "plan", field: "role", message: 'the role "wizard" does not exist' }] } }),
      }),
    );
    await open();
    fireEvent.click(await canvas().findByRole("button", { name: "Step plan" }));
    fireEvent.change(await page().findByLabelText("Prompt"), { target: { value: "" } });
    const alert = await page().findByRole("alert");
    expect(within(alert).getByText(/needs a prompt/)).toBeTruthy();

    fireEvent.change(page().getByLabelText("Prompt"), { target: { value: "plan it" } });
    fireEvent.click(page().getByRole("button", { name: "Save" }));
    expect(await page().findByText(/does not exist/)).toBeTruthy();
    expect(f.find("PUT", "/api/projects/p1/workflows/w1")).toHaveLength(1);
  });

  it("publishes a draft after saving it", async () => {
    const f = mockFetch(
      routes("member", {
        // the server answers with what it stored
        "PUT /api/projects/p1/workflows/w1": (c: { body: unknown }) => wf({ version: 4, nodes: (c.body as { nodes: WorkflowNode[] }).nodes }),
        "POST /api/projects/p1/workflows/w1/publish": wf({ version: 5, status: "published", publishedVersion: 5, nodes: wf().nodes.map((n) => (n.id === "plan" ? { ...n, title: "Plan it" } : n)) }),
      }),
    );
    await open();
    fireEvent.click(await canvas().findByRole("button", { name: "Step plan" }));
    fireEvent.change(await page().findByLabelText("Title"), { target: { value: "Plan it" } });
    fireEvent.click(page().getByRole("button", { name: "Publish" }));

    await waitFor(() => expect(f.find("POST", "/api/projects/p1/workflows/w1/publish")).toHaveLength(1));
    expect(f.find("PUT", "/api/projects/p1/workflows/w1")).toHaveLength(1);
    expect(f.find("POST", "/api/projects/p1/workflows/w1/publish")[0]?.body).toEqual({ version: 4 });
    expect(await page().findByText("published v5")).toBeTruthy();
  });

  it("configures a rework edge on a step", async () => {
    mockFetch(routes("member", { "GET /api/projects/p1/workflows": [wf({ nodes: [node("impl"), node("test", { kind: "script", prompt: undefined, role: undefined, command: "make test", dependsOn: ["impl"], pos: { x: 300, y: 40 } })] })] }));
    const { container } = await open();
    fireEvent.click(await canvas().findByRole("button", { name: "Step test" }));
    fireEvent.change(await page().findByLabelText("On failure"), { target: { value: "rework" } });
    expect((page().getByLabelText("Back to") as HTMLSelectElement).value).toBe("impl");
    fireEvent.change(page().getByLabelText("Rework rounds"), { target: { value: "2" } });
    // a dashed rework edge is drawn
    await waitFor(() => expect(container.querySelector("path[stroke-dasharray]")).not.toBeNull());
  });

  it("shows the version history and restores a version", async () => {
    const f = mockFetch(
      routes("member", {
        "GET /api/projects/p1/workflows/w1/history": [{ version: 2, workflow: wf({ version: 2 }), changedBy: "bob", at: T }],
        "POST /api/projects/p1/workflows/w1/restore": wf({ version: 4, name: "Old name" }),
      }),
    );
    await open();
    fireEvent.click(await page().findByRole("button", { name: /History/ }));
    fireEvent.click(await dialog().findByRole("button", { name: "Restore version 2" }));
    await waitFor(() => expect(f.find("POST", "/api/projects/p1/workflows/w1/restore")).toHaveLength(1));
    expect(f.find("POST", "/api/projects/p1/workflows/w1/restore")[0]?.body).toEqual({ version: 2 });
    await waitFor(() => expect((page().getByLabelText("Workflow name") as HTMLInputElement).value).toBe("Old name"));
  });
});

const step = (nodeId: string, state: string, extra: object = {}) => ({ nodeId, state, round: 1, ...extra });
const runDoc = (extra: Partial<WorkflowRun> = {}): WorkflowRun => ({
  id: "run1", projectId: "p1", workflowId: "w2", workflowName: "Published one", workflowVersion: 2, owner: "bob", state: "running", version: 4, createdAt: T, updatedAt: T,
  graph: [node("plan"), node("review", { kind: "gate", approvers: ["role:member"], dependsOn: ["plan"], pos: { x: 300, y: 40 } }), node("build", { dependsOn: ["review"], pos: { x: 560, y: 40 } })],
  steps: [step("plan", "done", { taskId: "t1", result: "the plan text", outputs: { plan: "step one" } }), step("review", "waiting"), step("build", "pending")],
  events: [{ at: T, kind: "gate", nodeId: "review", text: "waiting for role:member" }],
  inputs: { task: "ship login" },
  ...extra,
});
const detail = (run: WorkflowRun, o: Partial<WorkflowRunDetail> = {}): WorkflowRunDetail => ({ run, gates: [{ nodeId: "review", title: "review", approvers: ["role:member"], mayDecide: true }], canSteer: true, ...o });

describe("the runs view", () => {
  const view = (d: WorkflowRunDetail, extra: Record<string, unknown> = {}) =>
    mockFetch(routes("member", {
      "GET /api/projects/p1/workflow-runs/run1": d,
      "GET /api/projects/p1/tasks/t1": { task: { id: "t1" }, events: [{ id: 1, taskId: "t1", kind: "state", text: "done", at: T }] },
      ...extra,
    }));
  const canvas = () => within(screen.getByRole("group", { name: "Workflow canvas" }));
  const openRun = async () => {
    await renderApp("/p/p1?tab=workflows&run=run1");
    await screen.findByRole("group", { name: "Workflow canvas" });
  };

  it("lists the runs with their progress", async () => {
    mockFetch(routes("member", { "GET /api/projects/p1/workflow-runs?limit=50": [runDoc()] }));
    await renderApp("/p/p1?tab=workflows");
    fireEvent.mouseDown(await page().findByRole("tab", { name: "Runs" }), { button: 0 });
    fireEvent.click(page().getByRole("tab", { name: "Runs" }));
    expect(await page().findByText("1/3 steps")).toBeTruthy();
    expect(page().getByText("needs a decision")).toBeTruthy();
  });

  it("shows each step's state, the waiting gate and the finished step's result and task", async () => {
    view(detail(runDoc()));
    await openRun();

    expect(await canvas().findByText("Needs a decision")).toBeTruthy();
    expect(canvas().getByText("Done")).toBeTruthy();
    expect(canvas().getByRole("button", { name: "Step plan" }).getAttribute("data-state")).toBe("done");
    // the waiting gate is selected for a person who may decide it
    expect(await page().findByText(/This gate waits for you/)).toBeTruthy();
    fireEvent.click(canvas().getByRole("button", { name: "Step plan" }));
    expect(await page().findByText("the plan text")).toBeTruthy();
    expect(page().getByText("step one")).toBeTruthy();
    expect(page().getByRole("link", { name: /Open task/ }).getAttribute("href")).toBe("/#/p/p1?tab=tasks&task=t1");
    expect(await page().findByText(/Task log \(1\)/)).toBeTruthy();
  });

  it("approves a gate with a comment", async () => {
    const f = view(detail(runDoc()), { "POST /api/workflow-runs/run1/nodes/review/approve": runDoc() });
    await openRun();
    fireEvent.change(await page().findByLabelText("Approval comment"), { target: { value: "looks right" } });
    fireEvent.click(page().getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(f.find("POST", "/api/workflow-runs/run1/nodes/review/approve")).toHaveLength(1));
    expect(f.find("POST", "/api/workflow-runs/run1/nodes/review/approve")[0]?.body).toEqual({ comment: "looks right" });
  });

  it("sends a gate back only with feedback, and adds context", async () => {
    const f = view(detail(runDoc()), { "POST /api/workflow-runs/run1/nodes/review/send-back": runDoc(), "POST /api/workflow-runs/run1/nodes/review/add-context": runDoc() });
    await openRun();
    fireEvent.click(await page().findByRole("button", { name: "Send back" }));
    const go = page().getByRole("button", { name: "Send it back" }) as HTMLButtonElement;
    expect(go.disabled).toBe(true);
    fireEvent.change(page().getByLabelText(/What must change/), { target: { value: "add a rollback plan" } });
    fireEvent.click(go);
    await waitFor(() => expect(f.find("POST", "/api/workflow-runs/run1/nodes/review/send-back")).toHaveLength(1));
    expect(f.find("POST", "/api/workflow-runs/run1/nodes/review/send-back")[0]?.body).toEqual({ feedback: "add a rollback plan" });

    fireEvent.click(await page().findByRole("button", { name: "Add context" }));
    fireEvent.change(page().getByLabelText(/A note the next steps/), { target: { value: "mind the migration" } });
    fireEvent.click(page().getByRole("button", { name: "Add the note" }));
    await waitFor(() => expect(f.find("POST", "/api/workflow-runs/run1/nodes/review/add-context")).toHaveLength(1));
    expect(f.find("POST", "/api/workflow-runs/run1/nodes/review/add-context")[0]?.body).toEqual({ text: "mind the migration" });
  });

  it("does not offer the decision to someone who is not an approver", async () => {
    view(detail(runDoc(), { gates: [{ nodeId: "review", title: "review", approvers: ["alice"], mayDecide: false }] }));
    await openRun();
    fireEvent.click(await canvas().findByRole("button", { name: "Step review" }));
    expect(await page().findByText(/Waiting for role:member to decide/)).toBeTruthy();
    expect(page().queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("pauses, and confirms before cancelling", async () => {
    const f = view(detail(runDoc()), { "POST /api/workflow-runs/run1/pause": runDoc({ state: "paused" }), "POST /api/workflow-runs/run1/cancel": runDoc({ state: "cancelled" }) });
    await openRun();
    fireEvent.click(await page().findByRole("button", { name: /Pause/ }));
    await waitFor(() => expect(f.find("POST", "/api/workflow-runs/run1/pause")).toHaveLength(1));

    fireEvent.click(page().getByRole("button", { name: /Cancel run/ }));
    expect(f.find("POST", "/api/workflow-runs/run1/cancel")).toHaveLength(0);
    fireEvent.click(await screen.findByRole("button", { name: "Cancel the run" }));
    await waitFor(() => expect(f.find("POST", "/api/workflow-runs/run1/cancel")).toHaveLength(1));
  });

  it("reruns from a step with feedback and takes a step over", async () => {
    const f = view(detail(runDoc({ steps: [step("plan", "done", { result: "x" }), step("review", "failed", { error: "boom" }), step("build", "pending")] }), { gates: [] }), {
      "POST /api/workflow-runs/run1/rerun": runDoc(),
      "POST /api/workflow-runs/run1/retry": runDoc(),
      "POST /api/workflow-runs/run1/takeover": runDoc(),
    });
    await openRun();
    fireEvent.click(await canvas().findByRole("button", { name: "Step plan" }));

    fireEvent.click(await page().findByRole("button", { name: /Rerun from here/ }));
    fireEvent.change(await screen.findByLabelText(/differently/), { target: { value: "use another approach" } });
    fireEvent.click(screen.getByRole("button", { name: "Rerun" }));
    await waitFor(() => expect(f.find("POST", "/api/workflow-runs/run1/rerun")).toHaveLength(1));
    expect(f.find("POST", "/api/workflow-runs/run1/rerun")[0]?.body).toMatchObject({ nodeId: "plan", feedback: "use another approach" });

    fireEvent.click(canvas().getByRole("button", { name: "Step review" }));
    expect(await page().findByText("boom")).toBeTruthy();
    fireEvent.click(page().getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(f.find("POST", "/api/workflow-runs/run1/retry")).toHaveLength(1));
    expect(f.find("POST", "/api/workflow-runs/run1/retry")[0]?.body).toMatchObject({ nodeId: "review" });

    fireEvent.click(page().getByRole("button", { name: "Take over" }));
    const go = (await screen.findByRole("button", { name: "Mark as done" })) as HTMLButtonElement;
    expect(go.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("The result"), { target: { value: "I did it by hand" } });
    fireEvent.click(go);
    await waitFor(() => expect(f.find("POST", "/api/workflow-runs/run1/takeover")).toHaveLength(1));
    expect(f.find("POST", "/api/workflow-runs/run1/takeover")[0]?.body).toMatchObject({ nodeId: "review", result: "I did it by hand" });
  });

  it("a viewer sees the run without controls", async () => {
    view(detail(runDoc(), { canSteer: false, gates: [{ nodeId: "review", title: "review", approvers: ["role:member"], mayDecide: false }] }));
    await openRun();
    await canvas().findByRole("button", { name: "Step plan" });
    expect(page().queryByRole("button", { name: /Pause/ })).toBeNull();
    expect(page().queryByRole("button", { name: /Rerun from here/ })).toBeNull();
  });
});
