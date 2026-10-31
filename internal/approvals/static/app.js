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

const say = (text, isError) => {
  const el = document.getElementById("msg");
  el.textContent = text;
  el.className = isError ? "msg error" : "msg";
};

async function call(method, url, body) {
  const init = { method, headers: {} };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const resp = await fetch(url, init);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || resp.statusText);
  return data;
}

async function approve(button) {
  const id = button.dataset.approve;
  const edits = {};
  for (const ta of document.querySelectorAll("textarea[data-param]")) {
    if (ta.value !== ta.defaultValue) edits[ta.dataset.param] = ta.value;
  }
  const o = await call("POST", `/p/${id}/challenge`, { edits });
  say("Waiting for your passkey…");
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
  say("Sending…");
  await call("POST", `/p/${id}/approve`, { assertion });
  location.reload();
}

async function enroll(button) {
  const token = button.dataset.enroll;
  const o = await call("GET", `/enroll/${token}/options`);
  say("Waiting for your passkey…");
  const cred = await navigator.credentials.create({
    publicKey: {
      ...o,
      challenge: fromB64(o.challenge),
      user: { ...o.user, id: fromB64(o.user.id) },
      excludeCredentials: o.excludeCredentials.map((c) => ({ type: c.type, id: fromB64(c.id) })),
    },
  });
  const r = cred.response;
  const approver = await call("POST", `/enroll/${token}`, {
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
  button.hidden = true;
  document.getElementById("approver").textContent = JSON.stringify(approver, null, 2);
  document.getElementById("enrolled").hidden = false;
}

function bind(selector, action) {
  for (const button of document.querySelectorAll(selector)) {
    button.addEventListener("click", async () => {
      button.disabled = true;
      try {
        await action(button);
      } catch (e) {
        say(e.message || String(e), true);
      } finally {
        button.disabled = false;
      }
    });
  }
}

document.addEventListener("DOMContentLoaded", () => {
  bind("[data-approve]", approve);
  bind("[data-enroll]", enroll);
});
