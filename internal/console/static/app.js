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
  const sign = $("[data-sign]");
  if (sign) sign.textContent = unlocked ? "Approve →" : "Approve with passkey →";
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

async function review() {
  const id = $("#decide").dataset.id;
  const edits = {};
  for (const ta of $$("textarea[data-param]")) {
    if (ta.value !== ta.defaultValue) edits[ta.dataset.param] = ta.value;
  }
  const res = await call("POST", `/r/${id}/challenge`, { edits });
  pending = { id, nonce: res.nonce, options: res.options };
  // Server-rendered from the record the passkey signs; display values are
  // escaped and hidden characters marked there.
  $("#preview").innerHTML = res.preview;
  $("[data-review]").closest(".actions").hidden = true;
  $("#review").hidden = false;
  steps(2);
  say("");
  $("#review").scrollIntoView({ block: "start" });
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
      const box = el.closest("[data-connection], [data-change]");
      say(e.message || String(e), true, box ? $(".msg", box) : $("#msg"));
    } finally {
      el.disabled = false;
    }
  };
}

// The owner is in the middle of something a reload would lose.
const engaged = () =>
  pending !== null ||
  $("#review:not([hidden]), .linking:not([hidden]), button:disabled, input:focus, textarea:focus, select:focus") ||
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
  if (reviewButton) reviewButton.addEventListener("click", busy(reviewButton, review));
  const signButton = $("[data-sign]");
  if (signButton) signButton.addEventListener("click", busy(signButton, sign));
  const back = $("[data-back]");
  if (back) back.addEventListener("click", closeReview);

  for (const ta of $$("textarea[data-param]")) {
    ta.addEventListener("input", () => {
      closeReview();
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

  for (const button of $$("button[data-confirm]")) {
    button.addEventListener("click", confirmFirst(button, button.dataset.confirm, async () => button.form.submit()));
  }
}
