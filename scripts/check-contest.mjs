// Drives the contest Worker's own logic under Node, with no dependencies and
// no wrangler.
//
// The Go side is covered by go test, but the parts of worker.js that decide
// whether somebody gets a ballot have no Go equivalent to lean on, and are
// exactly where a mistake is invisible until a contest is already rigged:
// the OAuth session cookie and its CSRF state, the vote rules, and the
// hashing that has to agree with merlin's byte for byte. So this stubs D1
// and Discord and drives the real exported fetch handler.
//
// Deliberately not wired into CI, the same call scripts/check-lab.mjs made:
// adding Node to a Go pipeline for a hundred lines of glue is a worse trade
// than running this by hand when worker.js changes.
//
//   node scripts/check-contest.mjs

import assert from "node:assert/strict";
import worker from "../web/contest/worker.js";

const BOT_TOKEN = "test-bot-token";
// The same value TestTokenGoldenVector passes on the Go side. A golden
// vector under two different keys would compare nothing.
const LINK_KEY = "link-key";
const CLIENT_ID = "app-1";
const CLIENT_SECRET = "shh";

// --- a D1 good enough to run the four statements worker.js issues ----------

function makeDB() {
  const contests = new Map();
  const votes = new Set(); // "slug\0voter\0entry"

  const run = (sql, args) => {
    const s = sql.replace(/\s+/g, " ").trim();
    if (s.startsWith("INSERT INTO contests")) {
      const [slug, snapshot, phase, closes_at, updated_at] = args;
      const prev = contests.get(slug);
      contests.set(slug, { slug, snapshot, phase, closes_at, updated_at, frozen: prev ? prev.frozen : 0 });
      return { results: [] };
    }
    if (s.startsWith("UPDATE contests SET frozen")) {
      const c = contests.get(args[0]);
      if (c) c.frozen = 1;
      return { results: [] };
    }
    if (s.startsWith("SELECT snapshot, frozen FROM contests") ||
        s.startsWith("SELECT snapshot FROM contests")) {
      return { results: contests.has(args[0]) ? [contests.get(args[0])] : [] };
    }
    if (s.startsWith("SELECT entry_id FROM votes")) {
      const out = [];
      for (const k of votes) {
        const [slug, voter, entry] = k.split("\0");
        if (slug === args[0] && voter === args[1]) out.push({ entry_id: entry });
      }
      return { results: out };
    }
    if (s.startsWith("SELECT entry_id, COUNT(*)")) {
      const counts = new Map();
      for (const k of votes) {
        const [slug, , entry] = k.split("\0");
        if (slug !== args[0]) continue;
        counts.set(entry, (counts.get(entry) || 0) + 1);
      }
      return { results: [...counts].map(([entry_id, n]) => ({ entry_id, n })) };
    }
    if (s.startsWith("SELECT COUNT(*) AS votes")) {
      const voters = new Set();
      let n = 0;
      for (const k of votes) {
        const [slug, voter] = k.split("\0");
        if (slug !== args[0]) continue;
        n++; voters.add(voter);
      }
      return { results: [{ votes: n, voters: voters.size }] };
    }
    if (s.startsWith("DELETE FROM votes")) {
      for (const k of [...votes]) {
        const [slug, voter] = k.split("\0");
        if (slug === args[0] && voter === args[1]) votes.delete(k);
      }
      return { results: [] };
    }
    if (s.startsWith("INSERT OR IGNORE INTO votes")) {
      votes.add(args.join("\0"));
      return { results: [] };
    }
    throw new Error("unstubbed sql: " + s);
  };

  const stmt = (sql) => ({
    bind: (...args) => ({
      run: async () => run(sql, args),
      all: async () => run(sql, args),
      first: async () => (run(sql, args).results[0] ?? null),
      _sql: sql, _args: args,
    }),
  });

  return {
    prepare: stmt,
    batch: async (list) => { for (const s of list) await s.run(); },
    _contests: contests,
    _votes: votes,
  };
}

const env = () => ({
  DB: makeDB(), BOT_TOKEN, LINK_KEY,
  DISCORD_CLIENT_ID: CLIENT_ID, DISCORD_CLIENT_SECRET: CLIENT_SECRET,
  ASSETS: { fetch: async () => new Response("<html>ok</html>") },
});

// --- a Discord good enough for the OAuth callback -------------------------
//
// Membership is the interesting axis: `inGuild` false makes Discord answer
// 404 on the member endpoint, which is exactly what a stranger with a valid
// Discord login looks like.
const realFetch = globalThis.fetch;
function stubDiscord({ userID = "111222333444555666", inGuild = true, guild = "g1" } = {}) {
  globalThis.fetch = async (req, init) => {
    const url = typeof req === "string" ? req : req.url;
    if (url.includes("/oauth2/token")) {
      return new Response(JSON.stringify({ access_token: "at" }), {
        headers: { "content-type": "application/json" },
      });
    }
    if (url.includes(`/users/@me/guilds/${guild}/member`)) {
      if (!inGuild) return new Response("{}", { status: 404 });
      return new Response(JSON.stringify({
        nick: "ana", user: { id: userID, username: "ana", global_name: "ana" },
      }), { headers: { "content-type": "application/json" } });
    }
    throw new Error("unstubbed discord call: " + url);
  };
}
function unstubDiscord() { globalThis.fetch = realFetch; }

// --- signing, reimplemented here so the check is independent ---------------

const b64url = (bytes) =>
  Buffer.from(bytes).toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

async function macOf(payload) {
  const key = await crypto.subtle.importKey(
    "raw", new TextEncoder().encode(LINK_KEY),
    { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(payload)));
}

async function signed(payload) {
  return payload + "." + b64url(await macOf(payload));
}

// This has to equal workerClient.Hash in
// internal/plugins/contest/worker.go, byte for byte: merlin writes each
// entry's by_hash with it and the Worker computes the voter's with it, and
// the self-vote check compares the two.
async function hashID(userID) {
  return b64url((await macOf("id:" + userID)).slice(0, 16));
}

// Pinned against TestTokenGoldenVector on the Go side. If one moves without
// the other, this fails rather than a live contest quietly letting somebody
// vote for their own entry.
const GOLDEN_ID = "111222333444555666";
const GOLDEN_HASH = "TO2O8a5j_WwoeAVZamINPQ";

// A session cookie the Worker will accept, minted the way its callback does.
async function sessionCookie(slug, voterHash, ttl = 3600) {
  return "mv=" + await signed(`${slug}|${voterHash}|${Math.floor(Date.now() / 1000) + ttl}`);
}

// --- helpers --------------------------------------------------------------

const SLUG = "abcdefghijklmnop";
const GUILD = "g1";

const snapshot = (over = {}) => ({
  slug: SLUG, title: "neon cats", phase: "vote", guild: GUILD,
  submit_at: 1, vote_at: 2, results_at: Math.floor(Date.now() / 1000) + 3600,
  max_votes: 2,
  entries: [
    { id: "e1", by: "ana", by_hash: "hash-ana", title: "one", kind: "text", body: "hi" },
    { id: "e2", by: "bo", by_hash: "hash-bo", title: "two", kind: "text", body: "yo" },
    { id: "e3", by: "cy", by_hash: "hash-cy", title: "three", kind: "text", body: "ok" },
  ],
  prizes: [{ by: "ana", title: "a steam key" }],
  ...over,
});

const call = (e, path, init = {}) =>
  worker.fetch(new Request("https://x.test" + path, { redirect: "manual", ...init }), e);

const asBot = (body) => ({
  method: "PUT",
  headers: { authorization: "Bearer " + BOT_TOKEN, "content-type": "application/json" },
  body: JSON.stringify(body),
});

async function push(e, snap = snapshot()) {
  const res = await call(e, `/api/c/${SLUG}`, asBot(snap));
  assert.equal(res.status, 200, "snapshot push");
  return snap;
}

async function vote(e, cookie, picks) {
  const res = await call(e, `/api/c/${SLUG}/vote`, {
    method: "POST",
    headers: { "content-type": "application/json", ...(cookie ? { cookie } : {}) },
    body: JSON.stringify({ picks }),
  });
  return { status: res.status, body: await res.json() };
}

// Drive the real /login and /oauth/callback, so the state, the nonce cookie
// and the membership check are all exercised rather than assumed.
async function signIn(e, opts = {}) {
  stubDiscord({ guild: GUILD, ...opts });
  try {
    const loginRes = await call(e, `/c/${SLUG}/login`);
    assert.equal(loginRes.status, 302, "login should redirect to discord");
    const authorize = new URL(loginRes.headers.get("location"));
    const state = authorize.searchParams.get("state");
    const nonce = (loginRes.headers.get("set-cookie").match(/oa=([^;]*)/) || [])[1];

    const cbRes = await call(e, `/oauth/callback?code=abc&state=${encodeURIComponent(state)}`, {
      headers: { cookie: `oa=${nonce}` },
    });
    const setCookies = cbRes.headers.getSetCookie
      ? cbRes.headers.getSetCookie()
      : [cbRes.headers.get("set-cookie")];
    // Every cookie the callback set, not just the session one: the display
    // name rides in its own cookie and the page reads both together.
    const cookie = setCookies.filter(Boolean)
      .map((c) => c.split(";")[0])
      .filter((c) => c && !c.startsWith("oa="))
      .join("; ");
    return { status: cbRes.status, cookie: cookie.includes("mv=") ? cookie : "", authorize, state, nonce };
  } finally {
    unstubDiscord();
  }
}

const tests = [];
const test = (name, fn) => tests.push([name, fn]);

// --- the bot endpoints ----------------------------------------------------

test("the bot endpoints refuse an unsigned caller", async () => {
  const e = env();
  for (const [path, method] of [[`/api/c/${SLUG}`, "PUT"], [`/api/c/${SLUG}/close`, "POST"], [`/api/c/${SLUG}/stats`, "GET"]]) {
    const res = await call(e, path, { method, body: method === "PUT" ? "{}" : undefined });
    assert.equal(res.status, 401, path + " without a token");
  }
});

test("a wrong bot token is refused", async () => {
  const e = env();
  const res = await call(e, `/api/c/${SLUG}`, {
    method: "PUT",
    headers: { authorization: "Bearer nope" },
    body: JSON.stringify(snapshot()),
  });
  assert.equal(res.status, 401);
});

// --- oauth ----------------------------------------------------------------

// identify alone would let anybody with a Discord account vote in any
// server's contest. This is the check that makes it a members-only ballot.
test("the login redirect asks for the membership scope", async () => {
  const e = env();
  await push(e);
  const { authorize } = await signIn(e);
  assert.match(authorize.searchParams.get("scope"), /guilds\.members\.read/);
  assert.equal(authorize.searchParams.get("client_id"), CLIENT_ID);
  assert.equal(authorize.searchParams.get("redirect_uri"), "https://x.test/oauth/callback");
});

test("signing in sets a session and reveals who you are", async () => {
  const e = env();
  await push(e);
  const { status, cookie } = await signIn(e);
  assert.equal(status, 302, "callback should send you back to the page");
  assert.notEqual(cookie, "", "no session cookie was set");

  const me = await (await call(e, `/api/c/${SLUG}/me`, { headers: { cookie } })).json();
  assert.equal(me.signed_in, true);
  assert.equal(me.name, "ana");
});

test("somebody who is not in the server gets no ballot", async () => {
  const e = env();
  await push(e);
  const { status, cookie } = await signIn(e, { inGuild: false });
  assert.equal(status, 403, "a stranger with a valid discord login was let in");
  assert.equal(cookie, "", "a session was issued to a non-member");
});

// The state signature proves this Worker minted it. The nonce cookie proves
// it minted it for this browser, and without that a stolen state is a login
// CSRF.
test("a callback without the matching nonce cookie is refused", async () => {
  const e = env();
  await push(e);
  stubDiscord({ guild: GUILD });
  try {
    const loginRes = await call(e, `/c/${SLUG}/login`);
    const state = new URL(loginRes.headers.get("location")).searchParams.get("state");
    const res = await call(e, `/oauth/callback?code=abc&state=${encodeURIComponent(state)}`, {
      headers: { cookie: "oa=some-other-nonce" },
    });
    assert.equal(res.status, 400);
  } finally {
    unstubDiscord();
  }
});

test("a forged or expired state is refused", async () => {
  const e = env();
  await push(e);
  for (const state of ["", "junk", `${SLUG}|n|${Math.floor(Date.now() / 1000) + 600}.badsig`,
                       await signed(`${SLUG}|n|${Math.floor(Date.now() / 1000) - 10}`)]) {
    const res = await call(e, `/oauth/callback?code=abc&state=${encodeURIComponent(state)}`, {
      headers: { cookie: "oa=n" },
    });
    assert.equal(res.status, 400, JSON.stringify(state));
  }
});

// --- the session cookie ---------------------------------------------------

test("voting without a session is refused", async () => {
  const e = env();
  await push(e);
  assert.equal((await vote(e, "", ["e1"])).status, 401);
});

test("a tampered session cookie is refused", async () => {
  const e = env();
  await push(e);
  const good = await sessionCookie(SLUG, "hash-voter");
  const bad = good.slice(0, -1) + (good.at(-1) === "A" ? "B" : "A");
  assert.equal((await vote(e, bad, ["e1"])).status, 401);
});

// A session from one contest must not silently authorise a vote in the next
// one, which looks harmless right up until two contests run back to back.
test("a session for another contest is refused", async () => {
  const e = env();
  await push(e);
  const other = await sessionCookie("zzzzzzzzzzzzzzzz", "hash-voter");
  assert.equal((await vote(e, other, ["e1"])).status, 401);
});

test("an expired session is refused", async () => {
  const e = env();
  await push(e);
  assert.equal((await vote(e, await sessionCookie(SLUG, "hash-voter", -60), ["e1"])).status, 401);
});

test("garbage in the cookie is refused rather than throwing", async () => {
  const e = env();
  await push(e);
  for (const c of ["mv=", "mv=v1", "mv=a.b.c", "mv=!!!.???"]) {
    assert.equal((await vote(e, c, ["e1"])).status, 401, c);
  }
});

// --- the vote rules -------------------------------------------------------

test("one member gets one ballot no matter how many times they vote", async () => {
  const e = env();
  await push(e);
  const c = await sessionCookie(SLUG, "hash-voter");
  await vote(e, c, ["e1", "e2"]);
  await vote(e, c, ["e3"]);
  await vote(e, c, ["e3"]);
  const res = await call(e, `/api/c/${SLUG}/close`, {
    method: "POST", headers: { authorization: "Bearer " + BOT_TOKEN },
  });
  assert.deepEqual(await res.json(), { e3: 1 },
    "a member's later ballot must replace their earlier one, never add to it");
});

test("two different members each get a vote", async () => {
  const e = env();
  await push(e);
  await vote(e, await sessionCookie(SLUG, "hash-a"), ["e1"]);
  await vote(e, await sessionCookie(SLUG, "hash-b"), ["e1"]);
  const res = await call(e, `/api/c/${SLUG}/close`, {
    method: "POST", headers: { authorization: "Bearer " + BOT_TOKEN },
  });
  assert.deepEqual(await res.json(), { e1: 2 });
});

test("picks are capped at max_votes", async () => {
  const e = env();
  await push(e);
  const c = await sessionCookie(SLUG, "hash-voter");
  assert.equal((await vote(e, c, ["e1", "e2"])).status, 200);
  assert.equal((await vote(e, c, ["e1", "e2", "e3"])).status, 400);
});

test("you cannot vote for your own entry", async () => {
  const e = env();
  await push(e);
  const { status, body } = await vote(e, await sessionCookie(SLUG, "hash-ana"), ["e1"]);
  assert.equal(status, 400);
  assert.match(body.error, /your own/);
});

// The self-vote check compares merlin's by_hash against the hash the Worker
// computes from the OAuth'd id, so the two implementations must agree.
test("the hash agrees with the Go side and blocks a real self-vote", async () => {
  assert.equal(await hashID(GOLDEN_ID), GOLDEN_HASH,
    "hashID drifted from workerClient.Hash; update TestTokenGoldenVector too");

  const e = env();
  await push(e, snapshot({
    entries: [{ id: "e1", by: "ana", by_hash: GOLDEN_HASH, title: "one", kind: "text", body: "hi" },
              { id: "e2", by: "bo", by_hash: "hash-bo", title: "two", kind: "text", body: "yo" }],
  }));
  const { cookie } = await signIn(e, { userID: GOLDEN_ID });
  assert.equal((await vote(e, cookie, ["e1"])).status, 400, "signed-in author voted for themselves");
  assert.equal((await vote(e, cookie, ["e2"])).status, 200);
});

test("an unknown entry is refused", async () => {
  const e = env();
  await push(e);
  assert.equal((await vote(e, await sessionCookie(SLUG, "hash-voter"), ["nope"])).status, 400);
});

test("voting is refused outside the vote phase", async () => {
  const e = env();
  await push(e, snapshot({ phase: "submit" }));
  assert.equal((await vote(e, await sessionCookie(SLUG, "hash-voter"), ["e1"])).status, 409);
});

test("voting is refused past the deadline even before merlin freezes", async () => {
  const e = env();
  await push(e, snapshot({ results_at: Math.floor(Date.now() / 1000) - 1 }));
  assert.equal((await vote(e, await sessionCookie(SLUG, "hash-voter"), ["e1"])).status, 409,
    "the snapshot deadline is exact, the freeze is one tick late");
});

test("close freezes, so a vote after it is refused", async () => {
  const e = env();
  await push(e);
  await call(e, `/api/c/${SLUG}/close`, {
    method: "POST", headers: { authorization: "Bearer " + BOT_TOKEN },
  });
  assert.equal((await vote(e, await sessionCookie(SLUG, "hash-voter"), ["e1"])).status, 409);
});

test("a re-push after close does not thaw voting", async () => {
  const e = env();
  await push(e);
  await call(e, `/api/c/${SLUG}/close`, {
    method: "POST", headers: { authorization: "Bearer " + BOT_TOKEN },
  });
  await push(e); // merlin pushes the results snapshot right after closing
  assert.equal((await vote(e, await sessionCookie(SLUG, "hash-voter"), ["e1"])).status, 409,
    "frozen must survive a snapshot replace");
});

// --- what the browser is allowed to see -----------------------------------

test("the public view carries neither by_hash nor the guild id", async () => {
  const e = env();
  await push(e);
  const res = await call(e, `/api/c/${SLUG}/view`);
  const snap = await res.json();
  assert.equal("guild" in snap, false, "the guild id reached the browser");
  for (const entry of snap.entries) {
    assert.equal("by_hash" in entry, false, "by_hash reached the browser");
  }
  assert.equal(res.headers.get("x-robots-tag"), "noindex, nofollow");
});

test("me names your own entry without handing over a hash", async () => {
  const e = env();
  await push(e, snapshot({
    entries: [{ id: "e1", by: "ana", by_hash: GOLDEN_HASH, title: "one", kind: "text", body: "hi" }],
  }));
  const { cookie } = await signIn(e, { userID: GOLDEN_ID });
  const me = await (await call(e, `/api/c/${SLUG}/me`, { headers: { cookie } })).json();
  assert.equal(me.mine, "e1");
  assert.equal("you" in me, false, "me handed the browser a raw voter hash");
});

test("me says nothing to somebody not signed in", async () => {
  const e = env();
  await push(e);
  const me = await (await call(e, `/api/c/${SLUG}/me`)).json();
  assert.deepEqual(me, { signed_in: false });
});

test("the page is served noindex", async () => {
  const e = env();
  const res = await call(e, `/c/${SLUG}`);
  assert.equal(res.status, 200);
  assert.match(res.headers.get("x-robots-tag"), /noindex/);
});

test("an unknown contest is a 404, not a crash", async () => {
  const e = env();
  assert.equal((await call(e, `/api/c/${SLUG}/view`)).status, 404);
  assert.equal((await call(e, `/c/${SLUG}/login`)).status, 404);
});

test("stats counts people, not ballots", async () => {
  const e = env();
  await push(e);
  await vote(e, await sessionCookie(SLUG, "hash-a"), ["e1", "e2"]);
  await vote(e, await sessionCookie(SLUG, "hash-b"), ["e1"]);
  const res = await call(e, `/api/c/${SLUG}/stats`, {
    headers: { authorization: "Bearer " + BOT_TOKEN },
  });
  assert.deepEqual(await res.json(), { votes: 3, voters: 2 });
});

// --- run ------------------------------------------------------------------

let failed = 0;
for (const [name, fn] of tests) {
  try {
    await fn();
    console.log("ok   " + name);
  } catch (err) {
    failed++;
    console.log("FAIL " + name + "\n     " + (err && err.message));
  } finally {
    unstubDiscord();
  }
}
console.log(`\n${tests.length - failed}/${tests.length} passed`);
process.exit(failed ? 1 : 0);
