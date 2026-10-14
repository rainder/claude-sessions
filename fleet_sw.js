// FLEET page service worker (served at /fleet/sw.js, scope /fleet).
// The server replaces the VERSION placeholder with a hash of this file, the manifest and
// the icons, so any change to them is a new cache: activate drops every other
// fleet-* cache. Nothing to bump by hand.
//
// It caches only what is the same for every visitor: the manifest, the icons,
// and a copy of the app page for offline start. It never touches data or
// actions (state, chat, log, usage, say, start, accounts), login (any c= link,
// /fleet/login*), or anything outside /fleet. /fleet itself is network first
// and cached only when the response is the app (X-Fleet-App: 1), never the
// hop or login pages, which depend on auth.
"use strict";

const VERSION = "fleet-@VERSION@";
const SHELL = "/fleet";
const ASSETS = [
  "/fleet/manifest.webmanifest",
  "/fleet/icon-192.png",
  "/fleet/icon-512.png",
  "/fleet/icon-maskable-512.png",
  "/fleet/apple-touch-icon.png",
];
const NEVER = ["/fleet/state", "/fleet/chat", "/fleet/log", "/fleet/usage", "/fleet/say", "/fleet/start", "/fleet/login", "/fleet/accounts", "/fleet/account", "/fleet/sw.js"];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(VERSION).then((cache) => cache.addAll(ASSETS)).then(() => self.skipWaiting())
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith("fleet-") && k !== VERSION).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

function never(path) {
  return NEVER.some((p) => path === p || path.startsWith(p + "/") || (p === "/fleet/login" && path.startsWith(p)));
}

async function appShell(request) {
  let res;
  try {
    res = await fetch(request);
  } catch (err) {
    const hit = await caches.match(SHELL);
    if (hit) return hit;
    throw err;
  }
  if (res.status === 200 && res.headers.get("X-Fleet-App") === "1") {
    // A failed store (quota, private mode) must never cost the page itself.
    try {
      const cache = await caches.open(VERSION);
      await cache.put(SHELL, res.clone());
    } catch (_) {}
  }
  return res;
}

async function asset(request, path) {
  const hit = await caches.match(path);
  return hit || fetch(request);
}

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  if (url.pathname !== "/fleet" && !url.pathname.startsWith("/fleet/")) return;
  if (url.searchParams.has("c") || never(url.pathname)) return;
  if (req.mode === "navigate" && url.pathname === "/fleet") {
    event.respondWith(appShell(req));
    return;
  }
  if (ASSETS.includes(url.pathname)) {
    event.respondWith(asset(req, url.pathname));
  }
});
