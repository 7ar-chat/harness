// Behaviour of the documentation pages. Everything here is progressive: without the script the pages read the same
// (code blocks without copy buttons, every tab variant shown under its label, search absent).

// Copy buttons on code blocks.
for (const pre of document.querySelectorAll(".prose pre")) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "copy";
  btn.textContent = "Copy";
  btn.setAttribute("aria-label", "Copy code");
  btn.addEventListener("click", async () => {
    const text = (pre.querySelector("code") || pre).innerText.replace(/\n$/, "");
    try {
      await navigator.clipboard.writeText(text);
      btn.textContent = "Copied";
    } catch {
      btn.textContent = "Press Ctrl+C";
      const r = document.createRange();
      r.selectNodeContents(pre.querySelector("code") || pre);
      const s = getSelection();
      s.removeAllRanges();
      s.addRange(r);
    }
    setTimeout(() => (btn.textContent = "Copy"), 1600);
  });
  pre.appendChild(btn);
}

// Tabs: <div class="tabs"><div class="tab" data-label="..."> ... </div> ... </div>.
let tabId = 0;
for (const box of document.querySelectorAll(".tabs")) {
  const tabs = [...box.querySelectorAll(":scope > .tab")];
  if (tabs.length < 2) continue;
  const id = ++tabId;
  const list = document.createElement("div");
  list.className = "tablist";
  list.setAttribute("role", "tablist");
  const buttons = tabs.map((tab, i) => {
    const b = document.createElement("button");
    b.type = "button";
    b.role = "tab";
    b.id = `tab-${id}-${i}`;
    b.textContent = tab.dataset.label;
    tab.setAttribute("role", "tabpanel");
    tab.setAttribute("aria-labelledby", b.id);
    list.appendChild(b);
    return b;
  });
  const select = (n, focus) => {
    tabs.forEach((t, i) => {
      t.hidden = i !== n;
      buttons[i].setAttribute("aria-selected", String(i === n));
      buttons[i].tabIndex = i === n ? 0 : -1;
    });
    if (focus) buttons[n].focus();
  };
  buttons.forEach((b, i) => b.addEventListener("click", () => select(i)));
  list.addEventListener("keydown", (e) => {
    const cur = buttons.findIndex((b) => b.getAttribute("aria-selected") === "true");
    if (e.key === "ArrowRight") select((cur + 1) % buttons.length, true);
    else if (e.key === "ArrowLeft") select((cur - 1 + buttons.length) % buttons.length, true);
  });
  box.prepend(list);
  box.classList.add("ready");
  select(0);
}

// On-page table of contents: mark the heading being read.
const links = [...document.querySelectorAll(".docs-toc a")];
if (links.length && "IntersectionObserver" in window) {
  const byId = new Map(links.map((a) => [decodeURIComponent(a.hash.slice(1)), a]));
  const heads = [...byId.keys()].map((id) => document.getElementById(id)).filter(Boolean);
  const visible = new Set();
  const mark = () => {
    const first = heads.find((h) => visible.has(h));
    // when none is on screen keep the last one passed
    const passed = heads.filter((h) => h.getBoundingClientRect().top < 120).pop();
    const cur = first || passed;
    links.forEach((a) => a.classList.toggle("on", cur && byId.get(cur.id) === a));
  };
  const io = new IntersectionObserver((es) => {
    for (const e of es) (e.isIntersecting ? visible.add(e.target) : visible.delete(e.target));
    mark();
  }, { rootMargin: "-72px 0px -65% 0px" });
  heads.forEach((h) => io.observe(h));
  mark();
}

// Search over the build-time index (/docs-search.json: titles and headings of every page).
const form = document.querySelector("[data-search]");
if (form) {
  const input = form.querySelector("input");
  const box = form.querySelector(".docs-results");
  let index = null;
  let loading = null;
  let sel = -1;
  const load = () => (loading ||= fetch("/docs-search.json").then((r) => r.json()).then((j) => (index = j)).catch(() => (index = [])));
  const esc = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  const mark = (s, words) => {
    let out = esc(s);
    for (const w of words) out = out.replace(new RegExp(`(${w.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")})`, "ig"), "<mark>$1</mark>");
    return out;
  };
  const score = (rec, words) => {
    const t = rec.t.toLowerCase();
    const hay = `${t} ${(rec.d || "").toLowerCase()} ${(rec.h || "").toLowerCase()} ${rec.g.toLowerCase()}`;
    let s = 0;
    for (const w of words) {
      if (!hay.includes(w)) return 0;
      if (t === w) s += 12;
      else if (t.startsWith(w)) s += 8;
      else if (t.includes(w)) s += 5;
      else s += 1;
    }
    return s + (rec.h ? 0 : 2);
  };
  const render = () => {
    const q = input.value.trim().toLowerCase();
    sel = -1;
    if (!q || !index) { box.hidden = true; box.replaceChildren(); return; }
    const words = q.split(/\s+/);
    const hits = index.map((r) => [score(r, words), r]).filter((x) => x[0] > 0).sort((a, b) => b[0] - a[0]).slice(0, 12);
    box.innerHTML = hits.length
      ? hits.map(([, r], i) => `<li role="option" id="hit-${i}"><a href="${r.u}">${mark(r.t, words)}<small>${esc(r.h ? `${r.g} · ${r.h}` : r.g)}</small></a></li>`).join("")
      : `<li class="none">No page matches "${esc(input.value.trim())}".</li>`;
    box.hidden = false;
  };
  const move = (d) => {
    const items = [...box.querySelectorAll("li[role=option]")];
    if (!items.length) return;
    items.forEach((li) => li.removeAttribute("aria-selected"));
    sel = (sel + d + items.length) % items.length;
    items[sel].setAttribute("aria-selected", "true");
    items[sel].scrollIntoView({ block: "nearest" });
  };
  input.addEventListener("focus", () => load().then(render));
  input.addEventListener("input", () => load().then(render));
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); move(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); move(-1); }
    else if (e.key === "Enter") {
      const a = box.querySelector(sel >= 0 ? `li[aria-selected] a` : `li[role=option] a`);
      if (a) { e.preventDefault(); location.href = a.href; }
    } else if (e.key === "Escape") { input.value = ""; render(); input.blur(); }
  });
  form.addEventListener("submit", (e) => e.preventDefault());
  document.addEventListener("click", (e) => { if (!form.contains(e.target)) box.hidden = true; });
  document.addEventListener("keydown", (e) => {
    const t = e.target;
    const typing = t instanceof HTMLElement && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
    if (e.key === "/" && !typing && !e.metaKey && !e.ctrlKey) { e.preventDefault(); input.focus(); }
  });
}
