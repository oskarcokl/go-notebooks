"use strict";
const $ = (s, r = document) => r.querySelector(s);
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; };
let cells = [], vars = [], types = [], kernel = {};
const ui = new Map(); // cell id -> its elements
const pending = new Map(); // cell id -> save timer

async function api(method, path, body) {
  const r = await fetch("/api/" + path, { method, headers: { "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
  const data = r.status === 204 ? null : await r.json().catch(() => null);
  if (!r.ok) throw new Error((data && data.error) || r.statusText);
  return data;
}
function notice(text, kind) { const n = $("#notice"); n.hidden = !text; n.textContent = text || ""; n.className = kind || ""; if (text) n.scrollIntoView({ block: "nearest" }); }
const guard = f => async (...a) => { try { return await f(...a); } catch (e) { notice(e.message, "bad"); } };

// ---- Go highlighting
const KW = new Set("break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var".split(" "));
const TY = new Set("any bool byte comparable complex64 complex128 error float32 float64 int int8 int16 int32 int64 rune string uint uint8 uint16 uint32 uint64 uintptr true false nil iota".split(" "));
const TOKEN = /(\/\/[^\n]*|\/\*[\s\S]*?(?:\*\/|$))|(`[^`]*(?:`|$)|"(?:\\.|[^"\\\n])*"?|'(?:\\.|[^'\\\n])*'?)|(\b\d[\w.]*)|([A-Za-z_]\w*)(?=(\s*\()?)/g;
const esc = s => s.replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
function highlight(src) {
  let out = "", at = 0;
  for (const m of src.matchAll(TOKEN)) {
    out += esc(src.slice(at, m.index));
    const c = m[1] ? "com" : m[2] ? "str" : m[3] ? "n" : KW.has(m[4]) ? "kw" : TY.has(m[4]) ? "typ" : m[5] ? "fn" : "";
    out += c ? `<span class="${c}">${esc(m[0])}</span>` : esc(m[0]);
    at = m.index + m[0].length;
  }
  return out + esc(src.slice(at)) + "\n";
}

// ---- Markdown: a small, escaping renderer
function inline(s) {
  const codes = [];
  s = s.replace(/(`+)(.+?)\1/g, (_, t, c) => "\u0000" + (codes.push(esc(c.trim())) - 1) + "\u0000");
  s = esc(s)
    .replace(/!\[([^\]]*)\]\((https?:[^)\s]+)\)/g, '<img alt="$1" src="$2">')
    .replace(/\[([^\]]+)\]\(((?:https?:|mailto:|#|\/|\.)[^)\s]*)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>')
    .replace(/\*\*(.+?)\*\*/g, "<strong>$1</strong>").replace(/(^|[^\w*])\*([^*\s][^*]*?)\*(?!\w)/g, "$1<em>$2</em>")
    .replace(/(^|\W)_([^_\s][^_]*?)_(?!\w)/g, "$1<em>$2</em>").replace(/~~(.+?)~~/g, "<del>$1</del>");
  return s.replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${codes[i]}</code>`);
}
function markdown(src) {
  const lines = src.split("\n"); let out = "", i = 0;
  const list = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;
  while (i < lines.length) {
    const l = lines[i]; let m;
    if (!l.trim()) { i++; continue; }
    if ((m = l.match(/^\s*(```|~~~)\s*(\w*)/))) {
      const body = []; i++;
      while (i < lines.length && !lines[i].trim().startsWith(m[1])) body.push(lines[i++]);
      i++; const code = body.join("\n"); out += `<pre><code>${m[2] === "go" ? highlight(code) : esc(code)}</code></pre>`;
    } else if (/^( {4}|\t)/.test(l)) {
      const body = []; while (i < lines.length && (/^( {4}|\t)/.test(lines[i]) || !lines[i].trim())) body.push(lines[i++].replace(/^( {4}|\t)/, ""));
      out += `<pre><code>${esc(body.join("\n").trimEnd())}</code></pre>`;
    } else if ((m = l.match(/^(#{1,6})\s+(.*)$/))) { out += `<h${m[1].length}>${inline(m[2])}</h${m[1].length}>`; i++; }
    else if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(l)) { out += "<hr>"; i++; }
    else if (l.startsWith(">")) { const body = []; while (i < lines.length && lines[i].startsWith(">")) body.push(lines[i++].replace(/^>\s?/, "")); out += `<blockquote>${markdown(body.join("\n"))}</blockquote>`; }
    else if (l.includes("|") && /^\s*\|?[\s:|-]+-[\s:|-]*$/.test(lines[i + 1] || "")) {
      const row = r => r.trim().replace(/^\||\|$/g, "").split("|").map(c => inline(c.trim()));
      out += "<table><tr>" + row(l).map(c => `<th>${c}</th>`).join("") + "</tr>"; i += 2;
      while (i < lines.length && lines[i].includes("|")) out += "<tr>" + row(lines[i++]).map(c => `<td>${c}</td>`).join("") + "</tr>";
      out += "</table>";
    } else if ((m = l.match(list))) {
      const tag = /\d/.test(m[2]) ? "ol" : "ul", base = m[1].length; out += `<${tag}>`;
      while (i < lines.length && (m = lines[i].match(list)) && m[1].length >= base) {
        let item = inline(m[3]); i++; const sub = [];
        while (i < lines.length && lines[i].trim() && (lines[i].match(/^\s*/)[0].length > base)) sub.push(lines[i++]);
        if (sub.length) item += list.test(sub[0]) ? markdown(sub.map(s => s.replace(/^\s{1,4}/, "")).join("\n")) : " " + inline(sub.map(s => s.trim()).join(" "));
        out += `<li>${item}</li>`;
      }
      out += `</${tag}>`;
    } else { const p = []; while (i < lines.length && lines[i].trim() && !/^(#{1,6}\s|>|\s*(```|~~~))/.test(lines[i]) && (p.length === 0 || !list.test(lines[i]))) p.push(lines[i++]); out += `<p>${inline(p.join("\n"))}</p>`; }
  }
  return out;
}

// ---- Cells
function paint(u) { u.pre.innerHTML = highlight(u.area.value); }
function setSource(u, src) { if (u.area.value !== src) { const at = u.area.selectionStart; u.area.value = src; if (document.activeElement === u.area) u.area.setSelectionRange(at, at); } u.saved = src; paint(u); }

function makeCell(c) {
  const root = el("section", "cell"); root.id = "cell-" + c.id; root.dataset.id = c.id;
  const u = { id: c.id, kind: c.kind, root, saved: c.source };
  const add = el("div", "adder"); add.innerHTML = '<button data-add="code">+ Code</button><button data-add="markdown">+ Markdown</button>';
  add.dataset.before = c.id; u.adder = add;
  u.num = el("div", "num"); u.numText = el("span"); u.num.append(u.numText);
  if (c.kind === "code") { const b = el("button", "", "▶"); b.title = "Run (Shift+Enter)"; b.setAttribute("aria-label", "Run cell"); b.onclick = () => runCell(c.id); u.num.append(b); }
  const box = u.box = el("div", "box");
  const head = el("div", "head");
  u.title = el("input"); u.title.placeholder = c.kind === "code" ? "title" : "markdown"; u.title.setAttribute("aria-label", "Cell title");
  u.title.onchange = guard(() => api("PATCH", "cells/" + c.id, { title: u.title.value }));
  const tools = el("div", "tools");
  const tool = (label, title, f) => { const b = el("button", "", label); b.title = title; b.setAttribute("aria-label", title); b.onclick = guard(f); tools.append(b); };
  tool("↑", "Move up", async () => { await save(u); await api("POST", `cells/${c.id}/move`, { delta: -1 }); await load(); });
  tool("↓", "Move down", async () => { await save(u); await api("POST", `cells/${c.id}/move`, { delta: 1 }); await load(); });
  tool(c.kind === "code" ? "md" : "go", c.kind === "code" ? "Turn into Markdown" : "Turn into code", async () => { await save(u); await api("PATCH", "cells/" + c.id, { kind: c.kind === "code" ? "markdown" : "code" }); await load(); });
  tool("✕", "Delete cell", async () => { clearTimeout(pending.get(c.id)); pending.delete(c.id); await api("DELETE", "cells/" + c.id); await load(); });
  head.append(u.title, tools);
  const ed = el("div", "editor"); u.pre = el("pre"); u.area = el("textarea"); u.area.spellcheck = false; u.area.setAttribute("aria-label", "Cell source");
  u.area.autocapitalize = "off"; u.area.setAttribute("autocorrect", "off");
  ed.append(u.pre, u.area); box.append(head, ed); u.editor = ed;
  u.area.addEventListener("input", () => { paint(u); root.classList.toggle("edited", u.count > 0); clearTimeout(pending.get(c.id)); pending.set(c.id, setTimeout(guard(() => save(u)), 700)); });
  u.area.addEventListener("scroll", () => { u.pre.scrollLeft = u.area.scrollLeft; });
  u.area.addEventListener("blur", guard(async () => { await save(u); if (u.kind === "markdown") { u.editing = false; show(u); } }));
  u.area.addEventListener("keydown", e => key(e, u));
  root.append(u.num);
  if (c.kind === "markdown") {
    u.md = el("div", "md"); u.md.tabIndex = 0; u.md.title = "Click to edit";
    u.md.onclick = e => { if (e.target.closest("a")) return; edit(u); };
    u.md.onkeydown = e => { if (e.key === "Enter") { e.preventDefault(); edit(u); } };
    root.append(u.md);
  }
  u.warn = el("div"); u.out = el("div", "out"); root.append(box, u.warn, u.out);
  setSource(u, c.source); ui.set(c.id, u); return u;
}
function edit(u) { u.editing = true; show(u); u.area.focus(); }
function show(u) {
  if (u.kind !== "markdown") return;
  u.box.hidden = !u.editing; u.md.hidden = !!u.editing;
  u.md.innerHTML = markdown(u.area.value) || '<p class="empty">empty — click to write</p>';
}
function key(e, u) {
  const a = u.area;
  if (e.key === "Enter" && (e.shiftKey || e.ctrlKey || e.metaKey)) {
    e.preventDefault();
    const stay = e.ctrlKey || e.metaKey;
    if (u.kind === "code") runCell(u.id); else a.blur();
    if (!stay) next(u);
  } else if (e.key === "Tab") {
    e.preventDefault();
    if (e.shiftKey) { const s = a.value.lastIndexOf("\n", a.selectionStart - 1) + 1; if (a.value[s] === "\t") { a.setSelectionRange(s, s + 1); document.execCommand("delete"); } }
    else document.execCommand("insertText", false, "\t");
  } else if (e.key === "Enter") {
    const line = a.value.slice(a.value.lastIndexOf("\n", a.selectionStart - 1) + 1, a.selectionStart);
    const ind = line.match(/^\s*/)[0] + (u.kind === "code" && /[{(\[]\s*$/.test(line) ? "\t" : "");
    if (ind) { e.preventDefault(); document.execCommand("insertText", false, "\n" + ind); }
  } else if (e.key === "}" && /^\t+$/.test(a.value.slice(a.value.lastIndexOf("\n", a.selectionStart - 1) + 1, a.selectionStart))) {
    a.setSelectionRange(a.selectionStart - 1, a.selectionStart);
  } else if (e.key === "Escape") a.blur();
}
function next(u) {
  const i = cells.findIndex(c => c.id === u.id);
  for (const c of cells.slice(i + 1)) if (c.kind === "code") { ui.get(c.id).area.focus(); return; }
}
async function save(u) {
  clearTimeout(pending.get(u.id)); pending.delete(u.id);
  if (u.area.value === u.saved) return;
  const src = u.area.value; u.saved = src;
  await api("PATCH", "cells/" + u.id, { source: src });
}
const runCell = guard(async id => {
  const u = ui.get(id); clearTimeout(pending.get(id)); pending.delete(id);
  const src = u.area.value; u.saved = src;
  await api("POST", `cells/${id}/run`, { source: src });
});

function update(c) {
  const u = ui.get(c.id); if (!u) return;
  const i = cells.findIndex(x => x.id === c.id); if (i >= 0) cells[i] = c;
  const s = c.state; u.count = s.count;
  u.numText.textContent = c.num; u.root.dataset.status = s.status; u.root.classList.toggle("edited", s.edited);
  if (document.activeElement !== u.title) u.title.value = c.title;
  // Do not disturb typing: take the saved form only if nothing is unsaved.
  if (u.area.value === u.saved && u.area.value !== c.source && !pending.has(c.id)) setSource(u, c.source);
  show(u);
  u.warn.replaceChildren(); if (c.warning) u.warn.append(el("div", "badge warn", c.warning));
  if (s.stale && s.stale.length) u.warn.append(el("div", "badge stale", "stale: " + s.stale.join(", ") + " — re-run this cell"));
  const out = [];
  for (const o of s.outputs || []) out.push(el("pre", o.stream, o.text));
  if (s.result) out.push(el("pre", "result", s.result));
  for (const n of s.notices || []) out.push(el("pre", "note" + (n.includes("could not update") ? " attention" : ""), n));
  if (s.error) out.push(errorBox(s.error));
  const meta = [];
  if (s.status === "running" || s.status === "queued") meta.push(s.status + "…");
  else if (s.status && s.duration_ms !== undefined) meta.push(s.duration_ms >= 1000 ? (s.duration_ms / 1000).toFixed(2) + " s" : s.duration_ms.toFixed(1) + " ms");
  if (meta.length) out.push(el("div", "meta", meta.join(" ")));
  u.out.replaceChildren(...out);
}
function errorBox(e) {
  const stale = e.kind === "stale", b = el("div", "err" + (stale ? " stale" : ""), (e.line ? `line ${e.line}: ` : "") + e.message);
  const btns = el("div");
  if (stale) for (const id of new Set((e.stale || []).map(s => s.cell_id))) { const c = cells.find(c => c.id === id); if (c) { const x = el("button", "", "Re-run cell #" + c.num); x.onclick = () => runCell(id); btns.append(x); } }
  if (e.kind === "needs_sync") { const x = el("button", "", "Sync deps and replay"); x.onclick = syncDeps; btns.append(x); }
  if (btns.children.length) b.append(btns);
  return b;
}
function render(view) {
  cells = view.cells; $("#file").textContent = view.file; $("#path").textContent = view.path; document.title = view.file + " · go-notebooks";
  setKernel(view.kernel); if (view.error) notice(view.error, "bad");
  const keep = new Set(cells.map(c => c.id));
  for (const [id, u] of ui) if (!keep.has(id) || cells.find(c => c.id === id).kind !== u.kind) { u.root.remove(); u.adder.remove(); ui.delete(id); }
  const host = $("#cells"), order = []; let sec = null;
  for (const c of cells) {
    const u = ui.get(c.id) || makeCell(c);
    if (c.section !== sec && view.package === "main") {
      if (c.section === "main") order.push(el("div", "divider", "func main"));
      else if (c.section === "package" && sec === null || c.section === "package" && sec === "head") order.push(el("div", "divider", "package level"));
      else if (c.section === "post") order.push(el("div", "divider", "package level, after main"));
      sec = c.section;
    }
    order.push(u.adder, u.root);
  }
  // Move elements only where needed, so the focused editor keeps its focus.
  host.querySelectorAll(".divider").forEach(d => d.remove());
  order.forEach((n, i) => { if (host.children[i] !== n) host.insertBefore(n, host.children[i] || null); });
  cells.forEach(update);
}
let again = false, loading = null;
function load() { // coalesces bursts of events
  if (loading) { again = true; return loading; }
  loading = (async () => { do { again = false; render(await api("GET", "notebook")); } while (again); })().finally(() => { loading = null; });
  return loading;
}

// ---- Kernel and variables
function setKernel(k) { kernel = k; const p = $("#kernel"); p.dataset.s = k.status; p.textContent = { none: "no kernel", building: "building kernel…", ready: "ready", busy: "running", dead: "kernel died", error: "kernel error" }[k.status] || k.status; p.title = k.message || ((k.packages || []).length ? "Compiled in: " + k.packages.join(", ") : "Kernel status"); if (k.status === "error" || k.status === "dead") notice(k.message, "bad"); }
async function loadVars() {
  const d = await api("GET", "variables"); vars = d.variables; types = d.types;
  const stale = vars.filter(v => v.stale);
  $("#stale").hidden = !stale.length; $("#stale span").textContent = stale.length === 1 ? "1 stale variable" : stale.length + " stale variables";
  $("#nvars").textContent = vars.length || ""; $("#ntypes").textContent = types.length || "";
  const link = id => { const c = cells.find(c => c.id === id); if (!c) return el("span"); const a = el("a", "", "#" + c.num); a.href = "#cell-" + id; a.onclick = e => { e.preventDefault(); flash(id); }; return a; };
  $("#vars").replaceChildren(...(vars.length ? vars.map(v => {
    const r = el("div", "var" + (v.stale ? " stale" : "")), l = el("div", "l");
    l.append(el("b", "", v.name), el("span", "t", v.type));
    for (const x of v.versions || []) l.append(el("span", "badge", x));
    if (v.stale) l.append(el("span", "badge stale", "stale"));
    l.append(link(v.cell_id)); r.append(l, el("div", "v", v.value)); if (v.stale) r.append(el("div", "why", v.reason)); return r;
  }) : [el("div", "none", "Run a cell to see its variables.")]));
  $("#types").replaceChildren(...(types.length ? types.map(t => {
    const r = el("div", "var"), l = el("div", "l"); l.append(el("b", "", t.name), el("span", "badge", "v" + t.version), link(t.cell_id)); r.append(l);
    r.append(el("div", "v", t.fields ? t.fields.map(f => f.name + " " + f.type).join(", ") || "no fields" : t.schema));
    const h = (t.history || [])[(t.history || []).length - 1];
    if (h) { const ul = el("ul", "diff"); ul.append(el("li", "", `v${h.from} → v${h.to}`)); for (const d of h.details) ul.append(el("li", d.startsWith("added") ? "a" : d.startsWith("removed") ? "r" : "", (d.startsWith("added") ? "+ " : d.startsWith("removed") ? "− " : "~ ") + d.replace(/^(added|removed|changed) /, ""))); r.append(ul); }
    return r;
  }) : [el("div", "none", "Type declarations appear here with their version.")]));
}
function flash(id) { const u = ui.get(id); if (!u) return; u.root.scrollIntoView({ block: "center", behavior: "smooth" }); u.root.classList.add("flash"); setTimeout(() => u.root.classList.remove("flash"), 1200); }
const saveAll = () => Promise.all([...ui.values()].map(save));
const busy = (b, f) => guard(async () => { b.disabled = true; try { await saveAll(); await f(); } finally { b.disabled = false; } });
const syncDeps = guard(async () => { notice("Syncing dependencies…"); await saveAll(); const r = await api("POST", "kernel/sync-deps", { replay: true }); notice(r.rebuilt ? "Dependencies synced; the kernel was rebuilt and the cells that had run were run again." : "Dependencies are up to date."); await load(); await loadVars(); });
$("#run-all").onclick = e => busy(e.currentTarget, () => api("POST", "run-all"))();
$("#run-stale").onclick = e => busy(e.currentTarget, () => api("POST", "run-stale"))();
$("#restart").onclick = e => busy(e.currentTarget, async () => { notice(""); await api("POST", "kernel/restart"); await load(); await loadVars(); })();
$("#sync").onclick = e => busy(e.currentTarget, syncDeps)();
$("#build").onclick = e => busy(e.currentTarget, async () => { notice("go build…"); const r = await api("POST", "build"); notice(r.ok ? `go build: ok, compiles to a native binary (${(r.duration_ms / 1000).toFixed(1)} s)` : "go build failed:\n" + r.output, r.ok ? "good" : "bad"); })();
$("#kernel").onclick = () => notice((kernel.go ? kernel.go + "\n" : "") + "status: " + kernel.status + (kernel.message ? "\n" + kernel.message : "") + "\ncompiled-in packages: " + ((kernel.packages || []).join(", ") || "none"));
$("#toggle").onclick = e => { const off = $("#side").classList.toggle("off"); e.currentTarget.setAttribute("aria-expanded", !off); };
$("#theme").onclick = () => { const r = document.documentElement, dark = r.dataset.theme ? r.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches; r.dataset.theme = dark ? "light" : "dark"; try { localStorage.theme = r.dataset.theme; } catch { } };
try { if (localStorage.theme) document.documentElement.dataset.theme = localStorage.theme; } catch { }
document.addEventListener("click", guard(async e => {
  const b = e.target.closest("[data-add]"); if (!b) return;
  await saveAll(); const before = b.parentElement.dataset.before, i = before ? cells.findIndex(c => c.id === before) : cells.length;
  const c = await api("POST", "cells", { after: i > 0 ? cells[i - 1].id : "", kind: b.dataset.add, source: "" });
  await load(); const u = ui.get(c.id); if (u) edit(u), u.area.focus();
}));
addEventListener("beforeunload", e => { if (pending.size) { saveAll(); e.preventDefault(); } });

function listen() {
  const es = new EventSource("/api/events");
  es.onopen = guard(async () => { await load(); await loadVars(); });
  es.onmessage = guard(async m => {
    const ev = JSON.parse(m.data);
    if (ev.type === "cell") update(ev.view);
    else if (ev.type === "output") { const u = ui.get(ev.cell); if (!u) return; const last = [...u.out.querySelectorAll("pre")].pop(); if (last && last.className === ev.output.stream) last.textContent += ev.output.text; else { const p = el("pre", ev.output.stream, ev.output.text); const meta = u.out.querySelector(".meta"); u.out.insertBefore(p, meta); } }
    else if (ev.type === "kernel") setKernel(ev.kernel);
    else if (ev.type === "notebook") await load();
    else if (ev.type === "variables") { await loadVars(); await load(); }
  });
  es.onerror = () => setKernel({ status: "error", message: "lost connection to go-notebooks; retrying…", packages: [] });
}
listen();
