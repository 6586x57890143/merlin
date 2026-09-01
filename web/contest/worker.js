// The contest gallery and vote ledger, on a Cloudflare free-tier Worker.
//
// This thing is deliberately dumb. merlin owns the schema and pushes a
// complete snapshot of a contest on every change; the Worker stores that blob
// verbatim, serves it to browsers, and keeps a table of votes. It computes no
// standings, decides no winners, and holds no secret belonging to anybody.
// That is what makes it safe to put on somebody else's infrastructure.
//
// Who is voting is established by Discord OAuth, not by anything carried in a
// URL. That is what makes one ballot per member enforceable rather than
// merely encouraged: the vote is keyed on an account Discord just confirmed,
// and the scope asked for (guilds.members.read) is also what proves the voter
// is actually in the server running the contest. A shared link gets the
// recipient their own vote, never somebody else's.
//
// The Discord ID still never lands in the database. It is hashed the instant
// it arrives, with the same HMAC merlin uses, and only the hash is stored, so
// the votes table cannot be turned back into a list of who voted for what
// even by whoever holds it.
//
// Secrets, all via `wrangler secret put`:
//   BOT_TOKEN              bearer merlin sends on the /api/c/* calls it makes
//   LINK_KEY               must equal merlin's MERLIN_CONTEST_LINK_KEY
//   DISCORD_CLIENT_ID      the OAuth2 app, the same application as the bot
//   DISCORD_CLIENT_SECRET  from the same Developer Portal page

const JSON_HEADERS = { "content-type": "application/json; charset=utf-8" };

// The gallery is a public page showing members' work under their display
// names, on a server whose threat model is mass reporting. The slug is 128
// unguessable bits so nobody enumerates their way in, and this header is the
// other half: anyone with the link can look, and no crawler indexes it.
const PAGE_HEADERS = {
  "content-type": "text/html; charset=utf-8",
  "x-robots-tag": "noindex, nofollow, noarchive",
  "referrer-policy": "no-referrer",
};

const SESSION_TTL = 12 * 3600; // a contest is browsed over an evening
const STATE_TTL = 10 * 60;     // long enough to read Discord's consent screen

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const path = url.pathname;

    try {
      // Bot-facing. Every one of these is a write or a read of aggregate
      // state, so all three sit behind the bearer token.
      let m = path.match(/^\/api\/c\/([a-z2-7]{16,32})$/);
      if (m && request.method === "PUT") return await putSnapshot(m[1], request, env);

      m = path.match(/^\/api\/c\/([a-z2-7]{16,32})\/close$/);
      if (m && request.method === "POST") return await closeVoting(m[1], request, env);

      m = path.match(/^\/api\/c\/([a-z2-7]{16,32})\/stats$/);
      if (m && request.method === "GET") return await stats(m[1], request, env);

      // Browser-facing.
      m = path.match(/^\/api\/c\/([a-z2-7]{16,32})\/view$/);
      if (m && request.method === "GET") return await view(m[1], env);

      m = path.match(/^\/api\/c\/([a-z2-7]{16,32})\/me$/);
      if (m && request.method === "GET") return await whoami(m[1], request, env);

      m = path.match(/^\/api\/c\/([a-z2-7]{16,32})\/vote$/);
      if (m && request.method === "POST") return await castVote(m[1], request, env);

      // OAuth, two hops: /login sends you to Discord, /oauth/callback brings
      // you back and sets the cookie saying which member you are.
      m = path.match(/^\/c\/([a-z2-7]{16,32})\/login$/);
      if (m && request.method === "GET") return await login(m[1], url, env);

      if (path === "/oauth/callback" && request.method === "GET") {
        return await callback(request, url, env);
      }

      m = path.match(/^\/c\/([a-z2-7]{16,32})\/?$/);
      if (m && request.method === "GET") return await page(env);

      if (path === "/") return new Response("merlin contests", { status: 200 });
      return json({ error: "not found" }, 404);
    } catch (err) {
      // Never leak an internal message to a browser. merlin's own calls get
      // the detail because it puts them in an audit trail an operator reads;
      // a voter gets a status code.
      const bot = isBot(request, env);
      return json({ error: bot ? String((err && err.message) || err) : "something broke" }, 500);
    }
  },
};

function json(body, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: JSON_HEADERS });
}

// The dead end at the end of a sign in. It is the same paper and ink as the
// gallery, and deliberately not the same machinery: no drawably, no module,
// no fetch. Somebody reaches this page because something already went wrong,
// and the one thing it owes them is the sentence saying what, rendered
// before anything else has a chance to fail too.
//
// The sticker is the only request it makes, it is decorative, and it fails
// into whitespace. merlin looking put out is worth one 20KB image on a page
// that tells you your sign in did not work.
function html(message, status) {
  const mood = status >= 500 ? 'error' : 'warn';
  return new Response(
    '<!doctype html><meta charset=utf-8>' +
    '<meta name=viewport content="width=device-width,initial-scale=1">' +
    '<title>contest</title>' +
    '<style>' +
    ':root{color-scheme:light dark;--paper:#fdfbf6;--ink:#17150f;--muted:#6b6459}' +
    // The same four values index.html uses, copied rather than shared: this
    // page must render with no stylesheet, no script and no asset, so it
    // cannot read the gallery's tokens. Keep the two in step by hand.
    '@media(prefers-color-scheme:dark){:root{--paper:#1b211f;--ink:#eceae0;--muted:#a9b0a8}}' +
    'body{font:16px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;' +
    'margin:0;min-height:100vh;display:grid;place-items:center;padding:2rem;' +
    'background:var(--paper);color:var(--ink);text-align:center}' +
    'img{width:132px;height:auto;margin:0 auto .5rem;display:block}' +
    'p{margin:0;max-width:26rem}' +
    'small{display:block;margin-top:1.25rem;color:var(--muted)}' +
    '</style>' +
    '<body><div>' +
    '<img src="/stickers/merlin_' + mood + '.png" alt="">' +
    '<p>' + message + '</p>' +
    '<small>close this tab and go back to discord.</small>' +
    '</div></body>',
    { status, headers: PAGE_HEADERS });
}

// safeEqual is constant time. Comparing the bot token with === would leak it
// a byte at a time to anybody willing to make enough requests.
function safeEqual(a, b) {
  const enc = new TextEncoder();
  const x = enc.encode(a || ""), y = enc.encode(b || "");
  if (x.length !== y.length) return false;
  let diff = 0;
  for (let i = 0; i < x.length; i++) diff |= x[i] ^ y[i];
  return diff === 0;
}

function isBot(request, env) {
  const auth = request.headers.get("authorization") || "";
  return auth.startsWith("Bearer ") && safeEqual(auth.slice(7), env.BOT_TOKEN);
}

function requireBot(request, env) {
  return isBot(request, env) ? null : json({ error: "unauthorized" }, 401);
}

// --- identity -------------------------------------------------------------

function b64url(bytes) {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function b64urlDecode(s) {
  try {
    const pad = "=".repeat((4 - (String(s).length % 4)) % 4);
    const bin = atob(String(s).replace(/-/g, "+").replace(/_/g, "/") + pad);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  } catch {
    return null;
  }
}

async function hmac(env, bytes) {
  const key = await crypto.subtle.importKey(
    "raw", new TextEncoder().encode(env.LINK_KEY),
    { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", key, bytes));
}

// hashID has to produce byte for byte what workerClient.Hash produces in
// internal/plugins/contest/worker.go: merlin computes each entry's by_hash
// and this computes the voter's, and a self-vote is refused by comparing the
// two. TestTokenGoldenVector and scripts/check-contest.mjs pin the same
// vector from both sides, so a change to either fails a test rather than a
// contest.
async function hashID(env, userID) {
  const mac = await hmac(env, new TextEncoder().encode("id:" + userID));
  return b64url(mac.slice(0, 16));
}

// sign and unsign carry the session cookie and the OAuth state, which need
// the same thing: a short string this Worker can prove it wrote, with no
// table behind it. Payload is pipe separated and always ends in an expiry.
async function sign(env, payload) {
  return payload + "." + b64url(await hmac(env, new TextEncoder().encode(payload)));
}

async function unsign(env, signed) {
  const i = String(signed || "").lastIndexOf(".");
  if (i < 0) return null;
  const payload = String(signed).slice(0, i);
  const sig = b64urlDecode(String(signed).slice(i + 1));
  if (!sig) return null;

  const key = await crypto.subtle.importKey(
    "raw", new TextEncoder().encode(env.LINK_KEY),
    { name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
  // crypto.subtle.verify is constant time, which is why this verifies rather
  // than signing again and comparing strings.
  if (!(await crypto.subtle.verify("HMAC", key, sig, new TextEncoder().encode(payload)))) {
    return null;
  }
  const fields = payload.split("|");
  const exp = Number(fields[fields.length - 1]);
  if (!Number.isFinite(exp) || Math.floor(Date.now() / 1000) > exp) return null;
  return fields;
}

function cookies(request) {
  const out = {};
  for (const part of (request.headers.get("cookie") || "").split(";")) {
    const [k, ...v] = part.trim().split("=");
    if (k) out[k] = v.join("=");
  }
  return out;
}

// HttpOnly so page script cannot read it and an injected script cannot steal
// it; SameSite=Lax so it rides the OAuth redirect back from Discord but not a
// cross-site POST; Secure because this is only ever served over https.
function setCookie(name, value, maxAge) {
  return `${name}=${value}; HttpOnly; Secure; SameSite=Lax; Path=/; Max-Age=${maxAge}`;
}

// session returns the voter hash a request is authenticated as, or null.
//
// The cookie is bound to one contest on purpose. A session from one contest
// must not silently authorise a vote in the next one, which is the kind of
// thing that looks harmless right up until two contests run back to back.
async function session(request, env, slug) {
  const fields = await unsign(env, cookies(request).mv || "");
  if (!fields || fields.length !== 3 || fields[0] !== slug) return null;
  return fields[1];
}

// login sends the voter to Discord.
//
// identify alone would be enough to know who somebody is, and would let
// anybody with a Discord account vote in any server's contest.
// guilds.members.read is the scope that makes this a members-only ballot: it
// is what lets the callback ask Discord whether this account is actually in
// the guild running the contest, rather than taking the browser's word.
async function login(slug, url, env) {
  const row = await env.DB.prepare(
    `SELECT snapshot FROM contests WHERE slug = ?1`).bind(slug).first();
  if (!row) return json({ error: "not found" }, 404);

  const nonce = b64url(crypto.getRandomValues(new Uint8Array(16)));
  const exp = Math.floor(Date.now() / 1000) + STATE_TTL;
  const state = await sign(env, `${slug}|${nonce}|${exp}`);

  const authorize = new URL("https://discord.com/oauth2/authorize");
  authorize.searchParams.set("client_id", env.DISCORD_CLIENT_ID);
  authorize.searchParams.set("redirect_uri", url.origin + "/oauth/callback");
  authorize.searchParams.set("response_type", "code");
  authorize.searchParams.set("scope", "identify guilds.members.read");
  authorize.searchParams.set("state", state);

  return new Response(null, {
    status: 302,
    headers: {
      location: authorize.toString(),
      // The nonce also goes in a cookie. The signature proves this Worker
      // minted the state, but not that it minted it for *this* browser, and
      // without that a stolen state is a login CSRF.
      "set-cookie": setCookie("oa", nonce, STATE_TTL),
    },
  });
}

async function callback(request, url, env) {
  const state = await unsign(env, url.searchParams.get("state"));
  if (!state || state.length !== 3) {
    return html("that sign in link expired. go back and tap vote again.", 400);
  }
  const [slug, nonce] = state;
  if (!safeEqual(nonce, cookies(request).oa || "")) {
    return html("that sign in did not start in this browser.", 400);
  }

  const code = url.searchParams.get("code");
  if (!code) return html("discord did not send a code back.", 400);

  const tokenRes = await fetch("https://discord.com/api/v10/oauth2/token", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      client_id: env.DISCORD_CLIENT_ID,
      client_secret: env.DISCORD_CLIENT_SECRET,
      grant_type: "authorization_code",
      code,
      redirect_uri: url.origin + "/oauth/callback",
    }),
  });
  if (!tokenRes.ok) return html("discord would not complete the sign in.", 502);
  const tok = await tokenRes.json();

  const row = await env.DB.prepare(
    `SELECT snapshot FROM contests WHERE slug = ?1`).bind(slug).first();
  if (!row) return html("that contest is gone.", 404);
  const guild = JSON.parse(row.snapshot).guild;

  // The membership check, and the whole reason for the second scope. Discord
  // answers 404 for an account that is not in the guild, so a stranger with a
  // perfectly valid Discord login still gets no ballot.
  const memberRes = await fetch(
    `https://discord.com/api/v10/users/@me/guilds/${guild}/member`,
    { headers: { authorization: `Bearer ${tok.access_token}` } });
  if (memberRes.status === 404) {
    return html("you are not in that server, so you cannot vote in its contest.", 403);
  }
  if (!memberRes.ok) return html("discord would not confirm your membership.", 502);

  const member = await memberRes.json();
  const userID = member.user && member.user.id;
  if (!userID) return html("discord did not say who you are.", 502);

  // Hashed here and never stored raw. From this point on the Worker knows an
  // opaque string and nothing else about the person voting.
  const voter = await hashID(env, userID);
  const name = String(member.nick || member.user.global_name || member.user.username || "you").slice(0, 40);
  const exp = Math.floor(Date.now() / 1000) + SESSION_TTL;

  const headers = new Headers({ location: `/c/${slug}` });
  headers.append("set-cookie", setCookie("mv", await sign(env, `${slug}|${voter}|${exp}`), SESSION_TTL));
  headers.append("set-cookie", setCookie("nm", encodeURIComponent(name), SESSION_TTL));
  headers.append("set-cookie", setCookie("oa", "", 0));
  return new Response(null, { status: 302, headers });
}

// whoami is what the page asks on load, to decide between a sign in button
// and the picks somebody already made.
async function whoami(slug, request, env) {
  const voter = await session(request, env, slug);
  if (!voter) return json({ signed_in: false });

  const rows = await env.DB.prepare(
    `SELECT entry_id FROM votes WHERE slug = ?1 AND voter_hash = ?2`).bind(slug, voter).all();

  // Which entry is the viewer's own, resolved here rather than by handing
  // the page a hash to compare. /view strips by_hash for a reason, and
  // putting it back so the UI could grey out one card would undo that.
  const row = await env.DB.prepare(
    `SELECT snapshot FROM contests WHERE slug = ?1`).bind(slug).first();
  let mine = "";
  if (row) {
    const entry = (JSON.parse(row.snapshot).entries || []).find((e) => e.by_hash === voter);
    if (entry) mine = entry.id;
  }

  return json({
    signed_in: true,
    name: decodeURIComponent(cookies(request).nm || ""),
    picks: (rows.results || []).map((r) => r.entry_id),
    mine,
  });
}

// --- the bot's three calls ------------------------------------------------

// putSnapshot replaces the whole contest. A full replace rather than a merge
// because merlin owns the schema: a push landing after a missed one is still
// correct, with nothing to reconcile.
async function putSnapshot(slug, request, env) {
  const denied = requireBot(request, env);
  if (denied) return denied;

  const snap = await request.json();
  if (snap.slug !== slug) return json({ error: "slug mismatch" }, 400);

  await env.DB.prepare(`
    INSERT INTO contests (slug, snapshot, phase, closes_at, frozen, updated_at)
    VALUES (?1, ?2, ?3, ?4, 0, ?5)
    ON CONFLICT (slug) DO UPDATE SET
      snapshot = excluded.snapshot,
      phase = excluded.phase,
      closes_at = excluded.closes_at,
      updated_at = excluded.updated_at
  `).bind(slug, JSON.stringify(snap), snap.phase, snap.results_at | 0,
    Math.floor(Date.now() / 1000)).run();

  return json({ ok: true });
}

// closeVoting freezes and counts in one request, so no vote can land between
// the freeze and the count.
async function closeVoting(slug, request, env) {
  const denied = requireBot(request, env);
  if (denied) return denied;

  await env.DB.prepare(`UPDATE contests SET frozen = 1 WHERE slug = ?1`).bind(slug).run();
  const rows = await env.DB.prepare(
    `SELECT entry_id, COUNT(*) AS n FROM votes WHERE slug = ?1 GROUP BY entry_id`
  ).bind(slug).all();

  const tally = {};
  for (const r of rows.results || []) tally[r.entry_id] = r.n;
  return json(tally);
}

async function stats(slug, request, env) {
  const denied = requireBot(request, env);
  if (denied) return denied;

  const row = await env.DB.prepare(
    `SELECT COUNT(*) AS votes, COUNT(DISTINCT voter_hash) AS voters FROM votes WHERE slug = ?1`
  ).bind(slug).first();
  return json({ votes: (row && row.votes) || 0, voters: (row && row.voters) || 0 });
}

// --- the browser's two ----------------------------------------------------

// view is what the page fetches. It strips by_hash and the guild id before
// answering: the browser has no use for either, and by_hash is the one field
// that could be correlated across contests to work out who submitted what.
async function view(slug, env) {
  const row = await env.DB.prepare(
    `SELECT snapshot, frozen FROM contests WHERE slug = ?1`).bind(slug).first();
  if (!row) return json({ error: "not found" }, 404);

  const snap = JSON.parse(row.snapshot);
  snap.frozen = !!row.frozen;
  delete snap.guild;
  for (const e of snap.entries || []) delete e.by_hash;
  return new Response(JSON.stringify(snap), {
    status: 200,
    headers: { ...JSON_HEADERS, "x-robots-tag": "noindex, nofollow" },
  });
}

// castVote replaces a voter's whole ballot, so changing your mind is the same
// operation as voting and there is no half-updated state to reason about.
async function castVote(slug, request, env) {
  const voter = await session(request, env, slug);
  if (!voter) return json({ error: "sign in with discord first" }, 401);

  const row = await env.DB.prepare(
    `SELECT snapshot, frozen FROM contests WHERE slug = ?1`).bind(slug).first();
  if (!row) return json({ error: "not found" }, 404);
  if (row.frozen) return json({ error: "voting is closed" }, 409);

  const snap = JSON.parse(row.snapshot);
  if (snap.phase !== "vote") return json({ error: "voting is not open" }, 409);
  // The snapshot's own deadline is checked as well as the frozen flag,
  // because merlin freezes on its next tick and the deadline is exact.
  if (snap.results_at && Math.floor(Date.now() / 1000) > snap.results_at) {
    return json({ error: "voting is closed" }, 409);
  }

  const body = await request.json();
  const picks = Array.from(new Set(Array.isArray(body.picks) ? body.picks : []));
  if (picks.length > (snap.max_votes || 1)) {
    return json({ error: "too many picks" }, 400);
  }

  const byID = new Map((snap.entries || []).map((e) => [e.id, e]));
  for (const id of picks) {
    const entry = byID.get(id);
    if (!entry) return json({ error: "unknown entry" }, 400);
    // No voting for yourself. This is why entries carry by_hash at all, and
    // it is checked here rather than in the page because a check in the page
    // is a suggestion.
    if (entry.by_hash && entry.by_hash === voter) {
      return json({ error: "you cannot vote for your own entry" }, 400);
    }
  }

  const stmts = [
    env.DB.prepare(`DELETE FROM votes WHERE slug = ?1 AND voter_hash = ?2`).bind(slug, voter),
  ];
  for (const id of picks) {
    stmts.push(env.DB.prepare(
      `INSERT OR IGNORE INTO votes (slug, voter_hash, entry_id) VALUES (?1, ?2, ?3)`
    ).bind(slug, voter, id));
  }
  await env.DB.batch(stmts);

  return json({ ok: true, picks });
}

// page serves the single static asset. Every contest renders from the same
// document; the slug comes out of the URL client side and the content from
// /api/c/<slug>/view, so there is one page to cache and no templating.
async function page(env) {
  const res = await env.ASSETS.fetch(new Request("https://assets.local/index.html"));
  return new Response(res.body, { status: 200, headers: PAGE_HEADERS });
}
