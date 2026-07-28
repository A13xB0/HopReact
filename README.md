# HopReact

**Know when your MeshCore repeater goes quiet.**

Sign in with Discord, mark which repeaters and observers are yours, choose how
many hours of silence means trouble, and HopReact's bot sends you a direct
message when one stops being seen.

It reads a public [CoreScope](https://github.com/Kpa-clawbot/CoreScope)
instance — the same data behind
[HopReach](https://github.com/A13xB0/hopreach)'s coverage map — and does
nothing but watch it on your behalf.

> ⚠️ **Early scaffold.** The repository layout, config schema, CI and
> container build are in place; the service itself is being built out. It
> does not run yet.

## Why

On a real mesh, a dead repeater is easy to miss. On the network this was
built for, **448 of 660 repeaters had not been heard in over 24 hours** — one
more going quiet disappears into the noise. Nobody is watching your node for
you.

## No personal data

Discord is used for **both** sign-in and delivery, which means HopReact never
asks for an email address and never stores a password. It keeps your Discord
user ID and display name, and the OAuth token is discarded once you're signed
in.

## How it decides something is offline

CoreScope already tracks, per node, when it was last heard and when it last
appeared as a hop in a packet route. HopReact reads both:

- **Not heard** (default) — nothing at all from the node for your chosen
  number of hours.
- **Not relaying** (optional, per node) — the node is still alive but has
  stopped forwarding traffic. Off by default, because plenty of nodes have
  never relayed anything and it would be noise.

Observers are watched separately, on whether they are still reporting packets.

Alerts are deliberately quiet: one message when something goes down, one when
it comes back, and nothing in between — no matter how long it stays down.

## Quick start

```bash
git clone https://github.com/A13xB0/hopreact.git
cd hopreact
cp config.example.yaml config.yaml   # add your Discord app credentials
docker compose up --build
```

`config.example.yaml` documents every setting inline.

You will need a Discord application (client ID/secret, a bot token, and a
guild for the bot to live in). A bot can only DM someone it shares a server
with, so signing in adds you to that server — that is what the `guilds.join`
permission on the consent screen is for.

## License

[AGPL-3.0](https://www.gnu.org/licenses/agpl-3.0.html) plus the
[Commons Clause](https://commonsclause.com/) — see [`LICENSE`](LICENSE). In
short: use, copy and modify freely; if you distribute it or run a modified
version as a network service, publish the corresponding source under the same
license; you may **not** sell it or a service substantially based on it.
Provided free of charge, with no warranty and no support.
