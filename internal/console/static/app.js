"use strict";

const fromB64 = (s) => {
  s = s.replace(/-/g, "+").replace(/_/g, "/");
  while (s.length % 4) s += "=";
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
};

const toB64 = (buf) => {
  let s = "";
  for (const b of new Uint8Array(buf)) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
};

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

// A theme is stored only while it differs from the system's; this runs in
// <head> so a stored one applies before the first paint.
const themeMetas = $$('meta[name="theme-color"]');
const themeDefaults = themeMetas.map((m) => m.content);
const systemTheme = () => (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");

function applyTheme(theme) {
  const root = document.documentElement;
  if (theme) root.dataset.theme = theme;
  else delete root.dataset.theme;
  const bg = getComputedStyle(root).backgroundColor;
  themeMetas.forEach((m, i) => (m.content = theme ? bg : themeDefaults[i]));
}
applyTheme(localStorage.getItem("theme"));

function toggleTheme() {
  const next = (document.documentElement.dataset.theme || systemTheme()) === "dark" ? "light" : "dark";
  if (next === systemTheme()) localStorage.removeItem("theme");
  else localStorage.setItem("theme", next);
  applyTheme(next === systemTheme() ? null : next);
}

function say(text, isError, el = $("#msg")) {
  if (!el) return;
  el.textContent = text ? (isError ? "! " : "") + text : "";
  el.dataset.kind = isError ? "error" : "";
}

// The server's clock minus this device's, from the Date header, so a window
// ends at a time the server accepts even if this device's clock is off.
let clockSkew = 0;

async function call(method, url, body) {
  const init = { method, headers: {} };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(url, init);
  const date = Date.parse(resp.headers.get("Date"));
  if (date) clockSkew = date - Date.now();
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    const err = new Error(data.error || resp.statusText);
    err.status = resp.status;
    throw err;
  }
  return data;
}

async function getAssertion(o) {
  const cred = await navigator.credentials.get({
    publicKey: {
      challenge: fromB64(o.challenge),
      rpId: o.rpId,
      allowCredentials: o.allowCredentials.map((c) => ({ type: c.type, id: fromB64(c.id) })),
      userVerification: o.userVerification,
      timeout: o.timeout,
    },
  });
  const r = cred.response;
  const assertion = {
    id: cred.id,
    rawId: toB64(cred.rawId),
    type: cred.type,
    response: {
      authenticatorData: toB64(r.authenticatorData),
      clientDataJSON: toB64(r.clientDataJSON),
      signature: toB64(r.signature),
    },
  };
  if (r.userHandle) assertion.response.userHandle = toB64(r.userHandle);
  return assertion;
}

// An unlock window: a session key a passkey delegated to, so approving needs
// no touch until it ends. The key lives only in this page's memory, so while
// a window is open, links and forms load in place instead of navigating.
const WINDOW_S = 600;
let unlocked = null;

const utf8 = (s) => new TextEncoder().encode(s);

// Mirrors hashParts in the approval package.
async function hashParts(...parts) {
  const buf = new Uint8Array(parts.reduce((n, p) => n + 8 + p.length, 0));
  const view = new DataView(buf.buffer);
  let at = 0;
  for (const p of parts) {
    view.setBigUint64(at, BigInt(p.length));
    buf.set(p, at + 8);
    at += 8 + p.length;
  }
  return new Uint8Array(await crypto.subtle.digest("SHA-256", buf));
}

// newWindow makes a session key and the challenge whose assertion delegates
// to it and approves covers too.
async function newWindow(covers) {
  const pair = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]);
  const pub = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
  const end = Math.floor((Date.now() + clockSkew) / 1000) + WINDOW_S;
  const expires = new Date(end * 1000).toISOString().replace(".000Z", "Z");
  return {
    key: pair.privateKey,
    until: end * 1000 - clockSkew,
    delegation: { public_key: toB64(pub), expires_at: expires, covers: toB64(covers) },
    challenge: await hashParts(utf8("mandate-unlock-v1"), pub, utf8(expires), covers),
  };
}

async function sessionSign(win, challenge) {
  return toB64(await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, win.key, challenge));
}

function openWindow(win) {
  unlocked = win;
  showWindow();
}

function lock() {
  unlocked = null;
  showWindow();
}

const pad = (n) => String(n).padStart(2, "0");

function showWindow() {
  if (unlocked && Date.now() >= unlocked.until) unlocked = null;
  const box = $("[data-window]");
  if (box) {
    box.hidden = !unlocked;
    const s = unlocked ? Math.ceil((unlocked.until - Date.now()) / 1000) : 0;
    $("[data-window-left]", box).textContent = `${Math.floor(s / 60)}:${pad(s % 60)}`;
  }
  for (const b of $$("[data-sign], [data-approve-send]")) b.textContent = approveText();
  decideLabel();
  const last = $("#steps li:last-child");
  if (last) last.textContent = unlocked ? "approve" : "touch key";
}

// load shows url in this page, or navigates when there is no window to keep.
async function load(url, init = {}, replace = false) {
  if (!unlocked) {
    if (replace) location.replace(url);
    else location.assign(url);
    return;
  }
  const resp = await fetch(url, init);
  if (resp.status === 401 || new URL(resp.url).pathname === "/login") {
    lock();
    location.assign(resp.status === 401 ? "/login" : resp.url);
    return;
  }
  if (!(resp.headers.get("Content-Type") || "").startsWith("text/html")) {
    const data = await resp.json().catch(() => ({}));
    throw new Error(data.error || resp.statusText);
  }
  const doc = new DOMParser().parseFromString(await resp.text(), "text/html");
  document.title = doc.title;
  document.body.replaceWith(doc.body);
  if (replace) {
    history.replaceState(null, "", resp.url);
  } else {
    history.pushState(null, "", resp.url);
    scrollTo(0, 0);
  }
  bind();
}

const reload = () => (unlocked ? load(location.href, {}, true) : location.reload());

function loadLink(ev) {
  const a = ev.target.closest("a[href]");
  if (!unlocked || !a || ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
  const url = new URL(a.href);
  if (a.target || a.hasAttribute("download") || url.origin !== location.origin) return;
  ev.preventDefault();
  load(url.href).catch((e) => say(e.message, true));
}

function loadForm(ev) {
  const form = ev.target;
  if (!unlocked || ev.defaultPrevented) return;
  const url = new URL(form.action);
  if (url.pathname === "/logout") return lock();
  ev.preventDefault();
  const data = new URLSearchParams(new FormData(form));
  if (form.method === "get") url.search = data;
  const init = form.method === "get" ? {} : { method: "POST", body: data };
  load(url.href, init).catch((e) => say(e.message, true));
}

// The session cookie is SameSite=Strict, so a link opened from a chat app
// may arrive without it; a same-origin check finds an existing session.
async function resumeSession(next) {
  const resp = await fetch("/session").catch(() => null);
  if (resp && resp.status === 204) location.replace(next);
}

async function login(button) {
  const o = await call("POST", "/login/options");
  const win = await newWindow(fromB64(o.challenge));
  say("Waiting for your passkey…");
  const assertion = await getAssertion({ ...o, challenge: toB64(win.challenge) });
  const res = await call("POST", "/login", { assertion, unlock: win.delegation, next: button.dataset.login });
  win.delegation.assertion = assertion;
  openWindow(win);
  await load(res.next, {}, true);
}

async function passwordLogin(form) {
  const res = await call("POST", "/login", { password: form.elements.password.value, next: form.dataset.next });
  location.assign(res.next);
}

function steps(current) {
  const items = $$("#steps li");
  items.forEach((li, i) => {
    if (i < current) li.dataset.state = "done";
    else if (i === current) li.dataset.state = "current";
    else delete li.dataset.state;
  });
}

let pending = null;

function closeReview() {
  pending = null;
  const review = $("#review");
  if (!review || review.hidden) return;
  review.hidden = true;
  $("[data-review]").closest(".actions").hidden = false;
  steps(1);
}

function approveText() {
  return unlocked ? "Approve →" : "Approve with passkey →";
}

function edits() {
  const out = {};
  for (const ta of $$("textarea[data-param]")) {
    if (ta.value !== ta.defaultValue) out[ta.dataset.param] = ta.value;
  }
  return out;
}

// decideLabel says what the proposal page's button does: approve what the
// page shows, or after edits show the final text first.
function decideLabel() {
  const b = $("[data-review]");
  if (b) b.textContent = Object.keys(edits()).length ? "Review edits →" : approveText();
}

// prepare asks for the challenge that signs proposal id with edits and
// shows what it signs in preview, rendered by the server from that record;
// display values are escaped and hidden characters marked there.
async function prepare(id, changes, preview) {
  const res = await call("POST", `/r/${id}/challenge`, { edits: changes });
  pending = { id, nonce: res.nonce, options: res.options };
  if (preview) {
    preview.innerHTML = res.preview;
    preview.hidden = false;
  }
}

// decide approves the proposal as the page shows it in one step. Edited,
// the final text differs from what the agent wrote, so it is shown first.
async function decide() {
  const id = $("#decide").dataset.id;
  const changes = edits();
  if (!Object.keys(changes).length) {
    await prepare(id, changes, null);
    steps(2);
    return signNow();
  }
  await prepare(id, changes, $("#preview"));
  $("[data-review]").closest(".actions").hidden = true;
  $("#review").hidden = false;
  steps(2);
  say("");
  $("#review").scrollIntoView({ block: "start" });
}

// signNow signs what prepare just set up; a cancelled passkey leaves
// nothing waiting, so the page refreshes again.
async function signNow() {
  try {
    await sign();
  } finally {
    pending = null;
  }
}

async function sign() {
  if (!pending) return;
  const { id, nonce, options } = pending;
  const challenge = fromB64(options.challenge);
  let body;
  let opening = null;
  if (unlocked) {
    body = { nonce, delegation: unlocked.delegation, signature: await sessionSign(unlocked, challenge) };
  } else if ("passkey" in document.body.dataset) {
    // One touch approves this request and opens a window.
    opening = await newWindow(challenge);
    say("Waiting for your passkey…");
    opening.delegation.assertion = await getAssertion({ ...options, challenge: toB64(opening.challenge) });
    body = { nonce, delegation: opening.delegation, signature: await sessionSign(opening, challenge) };
  } else {
    say("Waiting for your passkey…");
    body = { nonce, assertion: await getAssertion(options) };
  }
  say("Sending…");
  pending = null;
  try {
    await call("POST", `/r/${id}/approve`, body);
  } catch (e) {
    // The window closed or its key was removed; the passkey opens a new one.
    if (e.status === 403) lock();
    closeReview();
    throw e;
  }
  if (opening) openWindow(opening);
  await reload();
}

async function addKey(form) {
  const o = await call("POST", "/keys/options", { name: form.elements.name.value });
  say("Touch your key…");
  const cred = await navigator.credentials.create({
    publicKey: {
      ...o,
      challenge: fromB64(o.challenge),
      user: { ...o.user, id: fromB64(o.user.id) },
      excludeCredentials: o.excludeCredentials.map((c) => ({ type: c.type, id: fromB64(c.id) })),
    },
  });
  const r = cred.response;
  await call("POST", "/keys", {
    id: cred.id,
    rawId: toB64(cred.rawId),
    type: cred.type,
    response: {
      clientDataJSON: toB64(r.clientDataJSON),
      attestationObject: toB64(r.attestationObject),
      transports: r.getTransports ? r.getTransports() : [],
    },
  });
  await reload();
}

async function approveKey(panel) {
  const id = panel.dataset.change;
  const msg = $(".msg", panel);
  const res = await call("POST", `/keys/changes/${id}/challenge`);
  say("Waiting for your passkey…", false, msg);
  const assertion = await getAssertion(res.options);
  await call("POST", `/keys/changes/${id}/approve`, { change: res.change, assertion });
  await reload();
}

// Access records take a passkey touch each time: an unlock window's
// session key can't sign them.
async function signAccess(body, msg) {
  const res = await call("POST", "/access/challenge", body);
  // Server-rendered: names escaped and hidden characters marked there.
  msg.innerHTML = `${res.summary}. Waiting for your passkey…`;
  msg.dataset.kind = "";
  const assertion = await getAssertion(res.options);
  say("Saving…", false, msg);
  const out = await call("POST", "/access/approve", { nonce: res.nonce, assertion });
  say("", false, msg);
  return out;
}

const hex = (buf) => [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, "0")).join("");

// The token is made and shown here; the console only signs its hash. The
// setup sheet then waits for the agent's first call through the gateway.
async function newAgent(form) {
  const name = form.elements.name.value;
  const token = hex(crypto.getRandomValues(new Uint8Array(32)));
  const tokenSHA256 = hex(await crypto.subtle.digest("SHA-256", utf8(token)));
  const res = await signAccess({ kind: "agent", name, token_sha256: tokenSHA256 }, $(".msg", form));
  const sheet = $("#agent-setup");
  $("[data-setup-token]", sheet).textContent = token;
  const copy = $("[data-copy-token]", sheet);
  copy.addEventListener("click", async () => {
    await navigator.clipboard.writeText(token);
    copy.textContent = "Copied";
    setTimeout(() => (copy.textContent = "Copy"), 2000);
    $("[data-clipboard-warning]", sheet).hidden = false;
  });
  for (const el of $$("[data-setup-name]", sheet)) el.textContent = name;
  form.hidden = true;
  sheet.hidden = false;
  const state = $("[data-setup-state]", sheet);
  const poll = async () => {
    if (!sheet.isConnected) return;
    const data = await call("GET", "/agents.json").catch(() => null);
    const agent = data && data.agents.find((a) => a.id === res.id);
    if (agent && agent.first_seen) {
      state.textContent = "Connected";
      return;
    }
    setTimeout(poll, POLL_MS);
  };
  poll();
}

// tree builds nested checkboxes for a chat and the chats under it, with
// labels the server escaped.
function tree(nodes, checkFirst) {
  const ul = document.createElement("ul");
  ul.className = "tree";
  nodes.forEach((n, i) => {
    const li = document.createElement("li");
    const label = document.createElement("label");
    const box = document.createElement("input");
    box.type = "checkbox";
    box.value = n.id;
    box.checked = (checkFirst && i === 0) || n.ticked;
    const text = document.createElement("span");
    text.className = "content";
    text.innerHTML = n.label;
    const meta = document.createElement("span");
    meta.className = "meta";
    meta.title = n.id;
    meta.textContent = n.meta;
    label.append(box, " ", text, " ", meta);
    li.append(label);
    if (n.children && n.children.length) li.append(tree(n.children, false));
    if (n.more) {
      const more = document.createElement("p");
      more.className = "hint";
      more.textContent = `and ${n.more} more under it, not shown`;
      li.append(more);
    }
    ul.append(li);
  });
  return ul;
}

const checked = (root) => $$("input[type=checkbox]:checked", root).map((b) => b.value);

// picker searches a connector's chats and adds each picked one, with the
// chats under it unticked.
function picker(root, connector, msg) {
  const results = $("[data-chat-results]", root);
  const picked = $("[data-picked]", root);
  const search = $("[data-chat-search]", root);
  const fail = (e) => say(e.message, true, msg);
  const pick = async (id) => {
    if ($$("input", picked).some((b) => b.value === id)) return;
    const q = new URLSearchParams({ connector: connector(), id });
    picked.append(tree([await call("GET", `/access/chats/tree?${q}`)], true));
  };
  const find = async () => {
    const q = new URLSearchParams({ connector: connector(), q: search.value });
    const res = await call("GET", `/access/chats?${q}`);
    say("", false, msg);
    results.replaceChildren(
      ...res.chats.map((c) => {
        const li = document.createElement("li");
        const add = document.createElement("button");
        add.type = "button";
        add.className = "compact";
        add.innerHTML = `+ ${c.label}`;
        add.addEventListener("click", () => pick(c.id).catch(fail));
        li.append(add);
        return li;
      }),
    );
  };
  let timer = 0;
  search.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(() => find().catch(fail), 250);
  });
  return {
    show: (on) => {
      const was = !root.hidden;
      root.hidden = !on;
      if (on && !was) find().catch(fail);
    },
    clear: () => {
      picked.replaceChildren();
      results.replaceChildren();
    },
    chats: () => checked(picked),
  };
}

// cardChats is what approving a card signs for chats: undefined keeps the
// request's choice, a list narrows it.
function cardChats(card) {
  if (card.dataset.chats === "pick") return card.picker.chats();
  if (card.dataset.chats === "named") return checked($("[data-tree]", card));
  if (card.picker && $("input[value=pick]", card).checked) return card.picker.chats();
  return undefined;
}

async function approveAccess(card) {
  const body = { kind: "grant", request: card.dataset.id };
  const forSel = $("select[name=for]", card);
  if (forSel) body.for = forSel.value;
  const horizon = $("select[name=horizon]", card);
  if (horizon) body.horizon = horizon.value;
  if (card.dataset.card === "routine") body.zone = timeZone();
  const chats = cardChats(card);
  if (chats !== undefined) {
    if (!chats.length) throw new Error("tick the chats to grant under Adjust");
    body.chats = chats;
  }
  await signAccess(body, $(".msg", card));
  await reload();
}

// requestCard wires Adjust, the picker for a request for every chat or one
// whose terms matched none, and keeps the Approve button naming what it
// signs; with no chat ticked it stays off.
function requestCard(card) {
  const approve = $("[data-approve-access]", card);
  const adjust = $("[data-adjust]", card);
  const panel = $("[data-adjust-panel]", card);
  if (adjust && panel) adjust.addEventListener("click", () => (panel.hidden = !panel.hidden));
  const pickerRoot = $("[data-chat-picker]", card);
  if (card.dataset.chats === "every" && pickerRoot) {
    card.picker = picker(pickerRoot, () => card.dataset.connector, $(".msg", card));
    for (const radio of $$("input[type=radio]", card)) {
      radio.addEventListener("change", () => card.picker.show($("input[value=pick]", card).checked));
    }
  }
  if (card.dataset.chats === "pick" && pickerRoot && adjust) {
    card.picker = picker(pickerRoot, () => card.dataset.connector, $(".msg", card));
    adjust.addEventListener("click", () => card.picker.show(!panel.hidden));
  }
  if (!approve || card.dataset.card === "extend") return;
  const hint = $("[data-tick-hint]", card);
  const optionText = (sel) => sel.selectedOptions[0].textContent;
  const label = () => {
    const forSel = $("select[name=for]", card);
    const parts = ["Approve", forSel ? optionText(forSel) : approve.dataset.span];
    const chats = cardChats(card);
    const ready = chats === undefined || chats.length > 0;
    if (card.dataset.chats === "none") parts.push("allowlisted reads");
    else if (chats === undefined) parts.push("every chat");
    else if (chats.length === 1) parts.push($(`input[value="${CSS.escape(chats[0])}"]`, card).closest("label").querySelector(".content").textContent);
    else parts.push(`${chats.length} chats`);
    const horizon = $("select[name=horizon]", card);
    if (horizon) parts.push(optionText(horizon));
    approve.textContent = ready ? parts.join(" · ") : "Approve";
    approve.disabled = !ready;
    if (hint) hint.hidden = ready;
  };
  card.addEventListener("change", label);
  card.addEventListener("click", (ev) => ev.target.closest("[data-picked]") && label());
  approve.label = label;
}

// countdowns shows time left on open windows and how long ago agents were
// seen, without a reload.
function countdowns() {
  for (const el of $$("[data-left]")) {
    const s = Math.max(0, Math.round((Date.parse(el.dataset.left) - Date.now()) / 1000));
    const d = Math.floor(s / 86400);
    const h = Math.floor(s / 3600) % 24;
    const m = Math.floor(s / 60) % 60;
    el.textContent = !s ? "closed" : d ? `${d}d ${h}h left` : `${h ? `${h}h ` : ""}${m}m left`;
  }
  for (const el of $$("[data-ago]")) {
    const m = Math.floor((Date.now() - Date.parse(el.dataset.ago)) / 60000);
    el.textContent = m < 1 ? "just now" : m < 60 ? `${m}m ago` : m < 1440 ? `${Math.floor(m / 60)}h ago` : `${Math.floor(m / 1440)}d ago`;
  }
}

// seenLive refreshes the agents' last-seen times from /agents.json.
async function seenLive() {
  if (!$("[data-agent] [data-ago]") || document.hidden) return;
  const data = await call("GET", "/agents.json").catch(() => null);
  for (const a of (data && data.agents) || []) {
    const el = $(`[data-agent="${CSS.escape(a.id)}"] [data-ago]`);
    if (el && a.last_seen) el.dataset.ago = a.last_seen;
  }
  countdowns();
}

const timeZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone;

async function newGrant(form) {
  const f = form.elements;
  const body = { kind: "grant", agent: f.agent.value, connector: f.connector.value, note: f.note.value };
  if (!form.forwardOnly()) {
    body.horizon = f.horizon.value;
    if (f.chats.value === "pick") {
      body.chats = form.picker.chats();
      if (!body.chats.length) throw new Error("pick at least one chat");
    }
  }
  if (f.when.value === "schedule") {
    const chosen = $$("input[name=day]:checked", form).map((b) => Number(b.value));
    const days = { every: [], weekdays: [1, 2, 3, 4, 5], chosen }[f.days.value];
    if (f.days.value === "chosen" && !days.length) throw new Error("choose at least one day");
    // f.length is the number of controls; the field is only reachable by name.
    body.schedule = { days, start: f.start.value, length: f.namedItem("length").value, zone: timeZone() };
  } else {
    body.for = f.for.value;
  }
  await signAccess(body, $(".msg", form));
  await reload();
}

// grantForm shows the parts of the new-grant form that apply to the
// chosen connector and timing.
function grantForm(form) {
  const f = form.elements;
  form.forwardOnly = () => "forwardOnly" in f.connector.selectedOptions[0].dataset;
  form.picker = picker($("[data-chat-picker]", form), () => f.connector.value, $(".msg", form));
  const show = () => {
    const fo = form.forwardOnly();
    $("[data-forward-hint]", form).hidden = !fo;
    for (const el of $$("[data-chats-part]", form)) el.hidden = fo;
    form.picker.show(!fo && f.chats.value === "pick");
    for (const el of $$("[data-when]", form)) el.hidden = el.dataset.when !== f.when.value;
    $("[data-days]", form).hidden = f.days.value !== "chosen";
  };
  f.connector.addEventListener("change", () => {
    form.picker.clear();
    show();
  });
  for (const radio of [...f.chats, ...f.when]) radio.addEventListener("change", show);
  f.days.addEventListener("change", show);
  $("[data-zone]", form).textContent = timeZone();
  show();
}

const POLL_MS = 2000;

function linkConnection(panel, args) {
  const name = panel.dataset.connection;
  const msg = $(".msg", panel);
  const controls = $("[data-link-controls]", panel);
  const box = $(".linking", panel);
  const qr = $(".qr-box", panel);
  const code = $(".code", panel);
  const confirm = $(".confirm", box);
  let stopped = false;
  let inFlight = false;
  let timer = 0;
  // The owner's answer to the step's question, sent instead of args once.
  let answer = null;

  const finish = () => {
    stopped = true;
    reload();
  };
  const step = async () => {
    if (stopped || !panel.isConnected) return;
    inFlight = true;
    try {
      const sent = answer;
      const s = await call("POST", `/connections/${name}/setup`, { args: sent ? { confirm: sent } : args });
      // The owner's inputs go once; the connector keeps the linking in progress.
      args = {};
      if (answer === sent) answer = null;
      if (s.done) return finish();
      box.hidden = false;
      qr.hidden = !s.qr_svg;
      // A server-drawn SVG of rects only.
      if (s.qr_svg) qr.innerHTML = s.qr_svg;
      code.hidden = !s.code;
      code.textContent = s.code || "";
      $(".instructions", box).textContent = s.instructions || "";
      confirm.hidden = !s.confirm || answer !== null;
      $(".question", confirm).textContent = s.confirm || "";
      say("", false, msg);
    } catch (e) {
      // Linking may finish between polls; the connector then refuses setup.
      const h = await call("GET", `/connections/${name}/health`).catch(() => null);
      if (h && h.state === "ok") return finish();
      stopped = true;
      box.hidden = true;
      controls.hidden = false;
      say(e.message, true, msg);
      return;
    } finally {
      inFlight = false;
    }
    timer = setTimeout(step, POLL_MS);
  };
  for (const button of $$("[data-answer]", confirm)) {
    button.onclick = () => {
      answer = button.dataset.answer;
      confirm.hidden = true;
      if (inFlight) return;
      clearTimeout(timer);
      step();
    };
  }
  controls.hidden = true;
  say("", false, msg);
  return step();
}

const CONFIRM_MS = 4000;

// The first click arms the button for a few seconds; a second click runs the
// action.
function confirmFirst(button, label, action) {
  const text = button.textContent;
  let timer = 0;
  const disarm = () => {
    clearTimeout(timer);
    timer = 0;
    button.textContent = text;
    button.classList.remove("danger");
  };
  return busy(button, async () => {
    if (!timer) {
      button.textContent = label;
      button.classList.add("danger");
      timer = setTimeout(disarm, CONFIRM_MS);
      return;
    }
    disarm();
    await action();
  });
}

async function unlinkConnection(panel) {
  await call("POST", `/connections/${panel.dataset.connection}/unlink`);
}

async function relink(panel) {
  await unlinkConnection(panel);
  $("[data-linked]", panel).hidden = true;
  const status = $("header .status", panel);
  status.textContent = "setup";
  status.dataset.tone = "wait";
  if ($("[data-link]", panel)) return linkConnection(panel, {});
  const controls = $("[data-link-controls]", panel);
  controls.hidden = false;
  $("input", controls).focus();
}

function busy(el, action) {
  return async (ev) => {
    if (ev) ev.preventDefault();
    if (el.disabled) return;
    el.disabled = true;
    try {
      await action();
    } catch (e) {
      const box = el.closest("[data-connection], [data-change], [data-access]");
      say(e.message || String(e), true, box ? $(".msg", box) : $("#msg"));
    } finally {
      el.disabled = false;
    }
  };
}

// The owner is in the middle of something a reload would lose.
const engaged = () =>
  pending !== null ||
  $(
    "#review:not([hidden]), .linking:not([hidden]), #agent-setup:not([hidden]), .stop-all details[open], " +
      "button:disabled:not([data-approve-access]), input:focus, textarea:focus, select:focus",
  ) ||
  $$("input:not([type=hidden]), textarea").some((el) => el.value !== el.defaultValue);

// Reloads after something changed elsewhere, e.g. a key approved on another
// device, once the owner isn't in the middle of something here.
function watch() {
  let stale = null;
  const check = async () => {
    const version = document.body.dataset.version;
    if (document.hidden || !version) return;
    if (stale !== version) {
      const res = await call("GET", "/version").catch(() => null);
      if (res !== null && res.version !== version) stale = version;
    }
    if (stale === version && !engaged()) reload();
  };
  setInterval(check, 3000);
  document.addEventListener("visibilitychange", check);
}

// Counts down to a resting connector's next try, then reloads to show it.
function countdown(el) {
  const until = Date.parse(el.dataset.until);
  // Only a countdown that ends while the page is open reloads it.
  const live = until > Date.now();
  const tick = () => {
    if (!el.isConnected) return clearInterval(timer);
    const s = Math.max(0, Math.ceil((until - Date.now()) / 1000));
    const h = Math.floor(s / 3600), m = Math.floor(s / 60) % 60, sec = s % 60;
    el.textContent = "in " + (h ? `${h}h ${pad(m)}m ${pad(sec)}s` : m ? `${m}m ${pad(sec)}s` : `${sec}s`);
    if (s === 0 && live && !engaged()) reload();
  };
  const timer = setInterval(tick, 1000);
  tick();
}

// Watches a connector that is connecting, e.g. just after linking, and
// reloads once it settles.
function settle(panel) {
  const poll = async () => {
    if (!panel.isConnected) return;
    const h = await call("GET", `/connections/${panel.dataset.connection}/health`).catch(() => null);
    if (h && h.state !== "connecting" && !engaged()) return reload();
    setTimeout(poll, POLL_MS);
  };
  setTimeout(poll, POLL_MS);
}

document.addEventListener("DOMContentLoaded", () => {
  bind();
  watch();
  setInterval(showWindow, 1000);
  setInterval(countdowns, 15000);
  setInterval(seenLive, 30000);
  document.addEventListener("click", loadLink);
  document.addEventListener("submit", loadForm);
  addEventListener("popstate", () => (unlocked ? load(location.href, {}, true).catch(() => location.reload()) : location.reload()));
});

// bind wires up the page's controls, on first load and after load swaps it in.
function bind() {
  pending = null;
  showWindow();
  $("[data-theme-toggle]").addEventListener("click", toggleTheme);
  for (const el of $$("[data-until]")) countdown(el);
  countdowns();
  const lockButton = $("[data-lock]");
  if (lockButton) lockButton.addEventListener("click", lock);

  const loginButton = $("[data-login]");
  if (loginButton) {
    resumeSession(loginButton.dataset.login);
    loginButton.addEventListener("click", busy(loginButton, () => login(loginButton)));
  }
  const passwordForm = $("#password-login");
  if (passwordForm) {
    resumeSession(passwordForm.dataset.next);
    passwordForm.addEventListener("submit", busy(passwordForm, () => passwordLogin(passwordForm)));
  }

  const reviewButton = $("[data-review]");
  if (reviewButton) reviewButton.addEventListener("click", busy(reviewButton, decide));
  const signButton = $("[data-sign]");
  if (signButton) signButton.addEventListener("click", busy(signButton, sign));
  const back = $("[data-back]");
  if (back) back.addEventListener("click", closeReview);

  for (const ta of $$("textarea[data-param]")) {
    ta.addEventListener("input", () => {
      closeReview();
      decideLabel();
      const hint = $(`[data-edited-hint="${ta.dataset.param}"]`);
      if (hint) hint.dataset.edited = ta.value !== ta.defaultValue ? "yes" : "";
    });
  }

  const add = $("#add-key");
  if (add) add.addEventListener("submit", busy(add, () => addKey(add)));
  for (const button of $$("[data-approve-key]")) {
    const panel = button.closest("[data-change]");
    button.addEventListener("click", busy(button, () => approveKey(panel)));
  }

  for (const panel of $$("[data-connection]")) {
    if (panel.dataset.state === "connecting") settle(panel);
    const link = $("[data-link]", panel);
    if (link) link.addEventListener("click", busy(link, () => linkConnection(panel, {})));
    for (const form of $$("[data-setup]", panel)) {
      form.addEventListener("submit", busy(form, () => linkConnection(panel, Object.fromEntries(new FormData(form)))));
      for (const ta of $$("textarea.secret", form)) {
        ta.addEventListener("keydown", (ev) => {
          if (ev.key === "Enter") {
            ev.preventDefault();
            form.requestSubmit();
          }
        });
      }
    }
    const unlink = $("[data-unlink]", panel);
    if (unlink) {
      unlink.addEventListener(
        "click",
        confirmFirst(unlink, "Confirm unlink", async () => {
          await unlinkConnection(panel);
          await reload();
        }),
      );
      const again = $("[data-relink]", panel);
      again.addEventListener("click", confirmFirst(again, "Confirm relink", () => relink(panel)));
    }
  }

  each($$("#new-agent"), (form) => form.addEventListener("submit", busy(form, () => newAgent(form))));
  each($$("[data-setup-done]"), (b) => b.addEventListener("click", () => location.reload()));
  each($$("[data-cancel-stop]"), (b) => b.addEventListener("click", () => (b.closest("details").open = false)));
  each($$("[data-approve-send]"), (button) => {
    const preview = $("[data-send-preview]", button.closest("[data-card]"));
    button.addEventListener("click", busy(button, async () => {
      // What the passkey signs shows on the card while it asks.
      await prepare(button.dataset.approveSend, {}, preview);
      await signNow();
    }));
  });
  each($$("[data-card]"), (card) => {
    if (card.dataset.card === "send") return;
    requestCard(card);
    const button = $("[data-approve-access]", card);
    if (button) {
      button.addEventListener("click", busy(button, () => approveAccess(card)));
      // busy enables it again afterwards; the ticks decide.
      button.addEventListener("click", () => setTimeout(() => button.label && button.label(), 0));
    }
  });
  each($$("[data-extend]"), (button) => {
    const body = { kind: "extend", grant: button.dataset.extend, for: "30m" };
    button.addEventListener("click", busy(button, async () => {
      await signAccess(body, $(".msg", button.closest("[data-access]")));
      await reload();
    }));
  });
  each($$("[data-resume]"), (button) => {
    const body = { kind: "resume", grant: button.dataset.resume };
    button.addEventListener("click", busy(button, async () => {
      await signAccess(body, $(".msg", button.closest("[data-access]")));
      await reload();
    }));
  });
  each($$("#new-grant"), (form) => {
    grantForm(form);
    form.addEventListener("submit", busy(form, () => newGrant(form)));
  });

  each($$("button[data-confirm]"), (button) => {
    button.addEventListener("click", confirmFirst(button, button.dataset.confirm, async () => button.form.submit()));
  });
}

// each sets up every element on its own, so one that fails leaves the
// rest of the page working.
function each(els, fn) {
  for (const el of els) {
    try {
      fn(el);
    } catch (e) {
      console.error(e);
    }
  }
}
