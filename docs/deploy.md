# Deploy

Push to `main`, CI does the rest. `lint-test`, `secret-scan`, `prose` and
`docker-build` have to pass, then `push-image` builds a multi-arch image and
pushes it to GHCR, and `deploy` SSHes into the VPS and runs
`docker compose -f docker-compose.prod.yml pull && up -d`.

The deploy pins the running image to the commit SHA in `deployed-tag.env` and
keeps whatever it replaced in `previous-tag.env`, which is what makes rolling
back a three-line job.

`docker-compose.prod.yml` is the same as the local compose file except `bot`
pulls the prebuilt image instead of building from source. Deploys never touch
`.env` or `config.yaml` on the host, those hold real secrets and you create them
there once by hand:

```sh
# once, on the VPS, in /home/deploy/merlin
cp .env.example .env
cp config.example.yaml config.yaml
```

## The contest gallery

Separate from the bot and deployed by hand, because it changes rarely and
nothing else depends on it. It is a Cloudflare Worker on the free plan: one
D1 database, one static HTML file, no bucket and no card on file.

```sh
scripts/build-contest.sh                    # copies her mood art in as stickers
cd web/contest
wrangler d1 create merlin-contests          # put the id in wrangler.jsonc
wrangler d1 execute merlin-contests --remote --file schema.sql
wrangler secret put BOT_TOKEN               # = MERLIN_CONTEST_WORKER_TOKEN
wrangler secret put LINK_KEY                # = MERLIN_CONTEST_LINK_KEY
wrangler secret put DISCORD_CLIENT_ID
wrangler secret put DISCORD_CLIENT_SECRET
wrangler deploy
```

It answers on `contest.melting.lol`, declared as a `custom_domain` route in
`wrangler.jsonc`. That zone is Cloudflare-managed on the same account, so the
DNS record and the certificate are created by the deploy; there is nothing to
add by hand. The `workers.dev` URL keeps working alongside it, deliberately:
contest announcements already posted to Discord carry that origin as literal
text, and a results gallery stays readable for 30 days past close.

Voting is Discord OAuth, so add `https://<your-worker>/oauth/callback` as a
redirect URI on the same application as the bot. The scopes are `identify`
and `guilds.members.read`, and the second one is the point: it is what proves
a voter is in your server rather than merely holding a Discord account.

Add the redirect URI for a new hostname **before** the deploy that starts
serving it. The Worker derives `redirect_uri` from whichever origin it was
reached on, so an unlisted host is not a warning, it is Discord rejecting every
ballot on a page that otherwise looks fine.

Whether to keep the *old* hostname listed is a question about live votes, not
about old links in general. Browsing takes no OAuth, so a gallery linked from
an announcement posted months ago opens either way. Only a ballot reached
through the old origin needs it, which means keeping it until the bot has been
restarted onto the new URL and no contest is still in its vote phase.

Then put the Worker's URL and the two shared secrets in the bot's `.env` and
restart it. Leave them empty and contests still run, just without a gallery.
`node scripts/check-contest.mjs` drives the Worker's own logic under Node if
you change it.

Repo secrets you'll need: `VPS_HOST` and `VPS_SSH_KEY`. GHCR auth uses the
workflow's own `GITHUB_TOKEN`, so there's no long-lived registry credential
sitting on the box. The SSH user needs to be able to run `docker compose`
without an interactive sudo prompt.
