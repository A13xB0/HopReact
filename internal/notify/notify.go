// Package notify drains the outbox: it turns queued transitions into
// Discord DMs.
//
// It is separate from the poller on purpose. The poller decides what is
// true; this decides how and when to say it. That split is what gives us
// retries with backoff, batching several transitions into one message, and —
// most importantly — a place to notice that a user has become unreachable
// and stop hammering Discord about it.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"hopreact/internal/discord"
	"hopreact/internal/poller"
	"hopreact/internal/store"
)

// maxAttempts is how many times a message is retried before being
// abandoned. With the store's exponential backoff this spans hours, which is
// long enough to ride out a Discord incident.
const maxAttempts = 8

// Sender is the part of the Discord client this package needs, narrowed so
// tests can substitute a recorder.
type Sender interface {
	SendDM(ctx context.Context, userID, content string) error
}

// Notifier drains queued notifications.
type Notifier struct {
	Store   *store.Store
	Sender  Sender
	Log     *slog.Logger
	BaseURL string
	Now     store.Clock
}

// DrainOnce sends everything currently due.
//
// Messages are grouped per user so someone whose five repeaters all went
// down in the same poll gets one DM listing them, not five pings. That is a
// courtesy, but it is also a structural limit on how much noise any single
// event can generate.
func (n *Notifier) DrainOnce(ctx context.Context, limit int) (sent int, err error) {
	pending, err := n.Store.PendingNotifications(ctx, limit)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	byUser := map[int64][]store.Notification{}
	var order []int64
	for _, msg := range pending {
		if _, seen := byUser[msg.UserID]; !seen {
			order = append(order, msg.UserID)
		}
		byUser[msg.UserID] = append(byUser[msg.UserID], msg)
	}

	for _, userID := range order {
		group := byUser[userID]
		content := n.render(group)
		discordID := group[0].DiscordID

		sendErr := n.Sender.SendDM(ctx, discordID, content)
		switch {
		case sendErr == nil:
			for _, m := range group {
				if err := n.Store.MarkNotificationSent(ctx, m.ID); err != nil {
					return sent, err
				}
			}
			sent += len(group)
			// A successful send is also proof the user is reachable again,
			// which clears any previously recorded delivery problem.
			if err := n.Store.SetDMStatus(ctx, userID, true, ""); err != nil {
				return sent, err
			}

		case errors.Is(sendErr, discord.ErrUndeliverable):
			// They left the alert server, blocked the bot, or turned off DMs
			// from server members. Retrying cannot fix any of those, so stop
			// and record it where the dashboard will show it — silence with
			// no explanation is the worst outcome here.
			n.Log.Warn("user is unreachable on Discord", "user", userID, "err", sendErr)
			if err := n.Store.SetDMStatus(ctx, userID, false,
				"Discord would not deliver the message — check you are still in the HopReact server and that DMs from server members are enabled"); err != nil {
				return sent, err
			}
			for _, m := range group {
				if err := n.Store.DropNotification(ctx, m.ID, sendErr.Error()); err != nil {
					return sent, err
				}
			}

		default:
			n.Log.Warn("DM failed, will retry", "user", userID, "err", sendErr)
			for _, m := range group {
				if m.Attempts+1 >= maxAttempts {
					if err := n.Store.DropNotification(ctx, m.ID,
						fmt.Sprintf("giving up after %d attempts: %v", m.Attempts+1, sendErr)); err != nil {
						return sent, err
					}
					continue
				}
				if err := n.Store.MarkNotificationFailed(ctx, m.ID, m.Attempts, sendErr.Error()); err != nil {
					return sent, err
				}
			}
		}
	}
	return sent, nil
}

// Run drains on every tick until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if _, err := n.DrainOnce(ctx, 100); err != nil {
				n.Log.Error("draining notifications failed", "err", err)
			}
		}
	}
}

// render turns one user's queued transitions into a single Discord message.
func (n *Notifier) render(group []store.Notification) string {
	type item struct {
		p poller.AlertPayload
	}
	var down, back []item
	for _, m := range group {
		var p poller.AlertPayload
		if err := json.Unmarshal([]byte(m.Payload), &p); err != nil {
			continue
		}
		if p.Kind == "recovered" {
			back = append(back, item{p})
		} else {
			down = append(down, item{p})
		}
	}
	sortItems := func(xs []item) {
		sort.Slice(xs, func(i, j int) bool { return xs[i].p.TargetName < xs[j].p.TargetName })
	}
	sortItems(down)
	sortItems(back)

	var b strings.Builder
	if len(down) > 0 {
		if len(down) == 1 {
			b.WriteString("🔴 **One of your nodes has gone quiet**\n")
		} else {
			fmt.Fprintf(&b, "🔴 **%d of your nodes have gone quiet**\n", len(down))
		}
		for _, it := range down {
			b.WriteString(line(it.p))
		}
	}
	if len(back) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if len(back) == 1 {
			b.WriteString("🟢 **Back online**\n")
		} else {
			fmt.Fprintf(&b, "🟢 **%d nodes are back online**\n", len(back))
		}
		for _, it := range back {
			b.WriteString(line(it.p))
		}
	}
	if n.BaseURL != "" {
		fmt.Fprintf(&b, "\n%s/ — manage your alerts", strings.TrimRight(n.BaseURL, "/"))
	}
	return b.String()
}

func line(p poller.AlertPayload) string {
	name := p.TargetName
	if strings.TrimSpace(name) == "" {
		name = shortKey(p.TargetKey)
	}
	what := "not heard"
	if p.Signal == string(store.SignalRelayed) {
		what = "not relaying"
	}
	kind := "repeater"
	if p.TargetKind == string("observer") {
		kind = "observer"
	}
	if p.Kind == "recovered" {
		return fmt.Sprintf("• **%s** (%s) — seen again\n", name, kind)
	}
	if p.LastSeenUnix == 0 {
		return fmt.Sprintf("• **%s** (%s) — %s, threshold %dh\n", name, kind, what, p.ThresholdHours)
	}
	last := time.Unix(p.LastSeenUnix, 0).UTC()
	return fmt.Sprintf("• **%s** (%s) — %s since %s (threshold %dh)\n",
		name, kind, what, last.Format("2006-01-02 15:04 UTC"), p.ThresholdHours)
}

func shortKey(k string) string {
	if len(k) <= 12 {
		return k
	}
	return k[:12] + "…"
}
