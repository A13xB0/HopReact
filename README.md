# HopReact

**Know when your MeshCore repeater goes quiet.**

Sign in with Discord, mark which repeaters and observers are yours, choose how
many hours of silence means trouble, and HopReact's bot sends you a direct
message when one stops being seen.

It reads a public [CoreScope](https://github.com/Kpa-clawbot/CoreScope)
instance — the same data behind
[HopReach](https://github.com/A13xB0/hopreach)'s coverage map — and does
nothing but watch it on your behalf.

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

## Status

Feature-complete and running: sign-in, watches, polling, the alert engine and
Discord delivery are all in place and tested. It has not yet been deployed
anywhere public.

## Quick start

```bash
git clone https://github.com/A13xB0/hopreact.git
cd hopreact
cp config.example.yaml config.yaml   # add your Discord app credentials
docker compose up --build
```

`config.example.yaml` documents every setting inline. Without Discord
credentials the service still starts and polls — you just can't sign in or
receive alerts, which is a deliberately useful state for looking around
first.

## What it does about noise

An alerting tool that cries wolf gets muted, and a muted tool is worthless.
So:

- **One message down, one message up.** A node offline for a week is one
  message, not one every five minutes.
- **Crossings must be confirmed.** A node hovering at your threshold doesn't
  produce a stream of up/down messages.
- **Nothing for something already broken.** Watch a node that's already quiet
  and you get no alert — and no "recovered" message later either, because you
  were never told it was down.
- **Never alerts on a node that has never relayed.** 189 of the repeaters on
  the live network have never appeared in a packet route; they haven't
  *stopped* doing anything.
- **Silence during our own outages.** If the upstream feed fails, returns
  implausibly little, or freezes, HopReact evaluates nothing rather than
  telling everyone their whole network died. And if a single poll would put
  an implausible number of watches into alert at once, a breaker withholds
  the lot and tells the operator instead.

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
