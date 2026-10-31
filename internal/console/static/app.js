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

function say(text, isError, el = $("#msg")) {
  if (!el) return;
  el.textContent = text ? (isError ? "! " : "") + text : "";
  el.dataset.kind = isError ? "error" : "";
}

async function call(method, url, body) {
  const init = { method, headers: {} };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(url, init);
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

// The session cookie is SameSite=Strict, so a link opened from a chat app
// may arrive without it; a same-origin check finds an existing session.
async function resumeSession(next) {
  const resp = await fetch("/session").catch(() => null);
  if (resp && resp.status === 204) location.replace(next);
}

async function login(button) {
  const o = await call("POST", "/login/options");
  say("Waiting for your passkey…");
  const assertion = await getAssertion(o);
  const res = await call("POST", "/login", { assertion, next: button.dataset.login });
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
  say("Waiting for your passkey…");
  const assertion = await getAssertion(options);
  say("Sending…");
  pending = null;
  await call("POST", `/r/${id}/approve`, { nonce, assertion });
  location.reload();
}

async function enroll(token) {
  const o = await call("GET", `/enroll/${token}/options`);
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
  const res = await call("POST", `/enroll/${token}`, {
    id: cred.id,
    rawId: toB64(cred.rawId),
    type: cred.type,
    response: {
      clientDataJSON: toB64(r.clientDataJSON),
      attestationObject: toB64(r.attestationObject),
      transports: r.getTransports ? r.getTransports() : [],
    },
  });
  say("");
  $("#kind").textContent = res.kind;
  $("#nix").textContent = res.nix;
  $("#approver").textContent = JSON.stringify(res.approver, null, 2);
  $("#enrolled").hidden = false;
}

async function addPasskey(form) {
  const res = await call("POST", "/passkeys", { name: form.elements.name.value });
  await enroll(res.token);
  form.hidden = true;
}

const POLL_MS = 2000;

function linkConnection(panel, args) {
  const name = panel.dataset.connection;
  const msg = $(".msg", panel);
  const box = $(".linking", panel);
  const qr = $(".qr-box", panel);
  const code = $(".code", panel);
  let stopped = false;

  const finish = () => {
    stopped = true;
    location.reload();
  };
  const step = async () => {
    if (stopped) return;
    try {
      const s = await call("POST", `/connections/${name}/setup`, { args });
      if (s.done) return finish();
      box.hidden = false;
      qr.hidden = !s.qr_svg;
      // A server-drawn SVG of rects only.
      if (s.qr_svg) qr.innerHTML = s.qr_svg;
      code.hidden = !s.code;
      code.textContent = s.code || "";
      $(".instructions", box).textContent = s.instructions || "";
      say("", false, msg);
    } catch (e) {
      // Linking may finish between polls; the connector then refuses setup.
      const h = await call("GET", `/connections/${name}/health`).catch(() => null);
      if (h && h.state === "ok") return finish();
      stopped = true;
      $("[data-poll]", box).textContent = "stopped";
      say(e.message, true, msg);
      return;
    }
    setTimeout(step, POLL_MS);
  };
  for (const el of $$("[data-link], [data-setup]", panel)) el.hidden = true;
  return step();
}

function busy(el, action) {
  return async (ev) => {
    if (ev) ev.preventDefault();
    if (el.disabled) return;
    el.disabled = true;
    try {
      await action();
    } catch (e) {
      say(e.message || String(e), true, el.closest("[data-connection]") ? $(".msg", el.closest("[data-connection]")) : $("#msg"));
    } finally {
      el.disabled = false;
    }
  };
}

document.addEventListener("DOMContentLoaded", () => {
  const loginButton = $("[data-login]");
  if (loginButton) {
    resumeSession(loginButton.dataset.login);
    loginButton.addEventListener("click", busy(loginButton, () => login(loginButton)));
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

  for (const button of $$("[data-enroll]")) {
    button.addEventListener("click", busy(button, async () => {
      await enroll(button.dataset.enroll);
      button.closest(".actions").hidden = true;
    }));
  }
  const add = $("#add-passkey");
  if (add) add.addEventListener("submit", busy(add, () => addPasskey(add)));

  for (const panel of $$("[data-connection]")) {
    const link = $("[data-link]", panel);
    if (link) link.addEventListener("click", busy(link, () => linkConnection(panel, {})));
    for (const form of $$("[data-setup]", panel)) {
      form.addEventListener("submit", busy(form, () => linkConnection(panel, Object.fromEntries(new FormData(form)))));
    }
  }

  for (const button of $$("[data-copy]")) {
    button.addEventListener("click", async () => {
      const label = button.textContent;
      await navigator.clipboard.writeText(document.getElementById(button.dataset.copy).textContent);
      button.textContent = "Copied ■";
      setTimeout(() => (button.textContent = label), 1500);
    });
  }
});
