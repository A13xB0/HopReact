// Package discord is HopReact's entire relationship with Discord: sign-in,
// adding people to the alert server, and sending the DMs.
//
// The constraint that shapes all of it: a bot can only DM someone it shares
// a server with. There is no OAuth scope for "DM this user whenever you
// like", and a user-installed app may only respond to interactions the user
// starts — neither can deliver an unprompted alert. So sign-in asks for
// guilds.join and puts the user into HopReact's own server, which is what
// makes a DM possible at all.
//
// The corollary is that leaving that server silently disables someone's
// alerts. That is why the read-only channel in the server says so, and why
// this package reports undeliverability as a first-class result
// (ErrUndeliverable) rather than as a generic failure to retry forever.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://discord.com/api/v10"

// Scopes requested at sign-in.
//
//   - identify: who you are (no email — we don't want it and don't ask).
//   - guilds.join: lets us add you to the alert server, which is the only
//     way the bot can DM you.
const oauthScopes = "identify guilds.join"

// ErrUndeliverable means Discord will not deliver to this user and retrying
// will not help: they have left the alert server, blocked the bot, or
// disabled DMs from server members. The caller records it against the user
// so the dashboard can say so, rather than queueing forever.
var ErrUndeliverable = errors.New("discord: cannot DM this user")

// discordErrCannotSendToUser is Discord's code for "cannot send messages to
// this user" — the one that means all three of the above.
const discordErrCannotSendToUser = 50007

// Client talks to Discord. Zero HTTP client means a sensible default.
type Client struct {
	ClientID     string
	ClientSecret string
	BotToken     string
	GuildID      string
	RedirectURI  string
	HTTP         *http.Client
}

// New returns a configured Client.
func New(clientID, clientSecret, botToken, guildID, redirectURI string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		ClientID: clientID, ClientSecret: clientSecret, BotToken: botToken,
		GuildID: guildID, RedirectURI: redirectURI, HTTP: hc,
	}
}

// AuthorizeURL is where sign-in sends the browser. state is the CSRF value
// echoed back to the callback.
func (c *Client) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", oauthScopes)
	q.Set("state", state)
	// Always show the consent screen. Being added to a Discord server is a
	// surprising thing to have happen silently, so the user should see
	// exactly what they are agreeing to every time.
	q.Set("prompt", "consent")
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}

// User is the part of Discord's user object we keep. Note the absence of
// email: the scopes never request it, so it is never returned.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	// GlobalName is the display name Discord shows now; Username is the
	// handle. Preferring GlobalName matches what people see elsewhere.
	GlobalName string `json:"global_name"`
	Avatar     string `json:"avatar"`
}

// DisplayName is what to show in the UI.
func (u User) DisplayName() string {
	if strings.TrimSpace(u.GlobalName) != "" {
		return u.GlobalName
	}
	return u.Username
}

// Exchange trades an OAuth code for an access token, fetches the user, and
// adds them to the alert server.
//
// The access token is used here and then dropped: everything afterwards is
// done with the bot token, so there is no user credential worth storing.
func (c *Client) Exchange(ctx context.Context, code string) (User, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.RedirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var tok struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if err := c.do(req, &tok); err != nil {
		return User{}, fmt.Errorf("discord: exchanging code: %w", err)
	}
	if tok.AccessToken == "" {
		return User{}, errors.New("discord: no access token returned")
	}

	user, err := c.currentUser(ctx, tok.AccessToken)
	if err != nil {
		return User{}, err
	}
	// Best-effort: an already-member returns 204 and a failure here must not
	// block sign-in. Whether the bot can actually reach them is established
	// separately by CheckMembership, which the caller surfaces in the UI.
	_ = c.AddToGuild(ctx, user.ID, tok.AccessToken)
	return user, nil
}

func (c *Client) currentUser(ctx context.Context, accessToken string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/users/@me", nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	var u User
	if err := c.do(req, &u); err != nil {
		return User{}, fmt.Errorf("discord: fetching user: %w", err)
	}
	return u, nil
}

// AddToGuild puts the user into the alert server. Requires the guilds.join
// scope on their token AND the bot already being a member of that guild.
func (c *Client) AddToGuild(ctx context.Context, userID, accessToken string) error {
	body, _ := json.Marshal(map[string]string{"access_token": accessToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/guilds/%s/members/%s", apiBase, c.GuildID, userID),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.BotToken)
	req.Header.Set("Content-Type", "application/json")
	// 201 created, 204 already a member — both fine.
	return c.do(req, nil)
}

// CheckMembership reports whether the user is currently in the alert server.
//
// Worth doing proactively rather than waiting for a send to fail: someone
// who quietly left has working-looking alerts that will never arrive, and
// finding that out at the moment their repeater dies is exactly the wrong
// time.
func (c *Client) CheckMembership(ctx context.Context, userID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/guilds/%s/members/%s", apiBase, c.GuildID, userID), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bot "+c.BotToken)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	default:
		return false, fmt.Errorf("discord: checking membership: status %d", resp.StatusCode)
	}
}

// SendDM opens a DM channel with the user and posts content.
//
// Returns ErrUndeliverable when Discord says it will not deliver, so the
// caller can stop retrying and tell the user why on the dashboard instead.
func (c *Client) SendDM(ctx context.Context, userID, content string) error {
	channelID, err := c.createDMChannel(ctx, userID)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"content": content})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/channels/%s/messages", apiBase, channelID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.BotToken)
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}

func (c *Client) createDMChannel(ctx context.Context, userID string) (string, error) {
	body, _ := json.Marshal(map[string]string{"recipient_id": userID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/users/@me/channels", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+c.BotToken)
	req.Header.Set("Content-Type", "application/json")

	var ch struct {
		ID string `json:"id"`
	}
	if err := c.do(req, &ch); err != nil {
		return "", err
	}
	if ch.ID == "" {
		return "", errors.New("discord: no DM channel id returned")
	}
	return ch.ID, nil
}

// apiError is Discord's error body.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// do performs a request, honours one rate-limit retry, and decodes into dst
// when dst is non-nil.
func (c *Client) do(req *http.Request, dst any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 429 is normal on Discord and self-describing: it says how long to
	// wait. Retrying once inline covers the ordinary burst; anything worse
	// falls through to the outbox's own backoff rather than sleeping here
	// and holding up the drainer.
	if resp.StatusCode == http.StatusTooManyRequests {
		retry := retryAfter(resp)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if retry > 0 && retry <= 10*time.Second && req.GetBody != nil {
			select {
			case <-time.After(retry):
			case <-req.Context().Done():
				return req.Context().Err()
			}
			body, err := req.GetBody()
			if err != nil {
				return err
			}
			retryReq := req.Clone(req.Context())
			retryReq.Body = body
			return c.doOnce(retryReq, dst)
		}
		return fmt.Errorf("discord: rate limited, retry after %s", retry)
	}
	return c.finish(resp, dst)
}

func (c *Client) doOnce(req *http.Request, dst any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.finish(resp, dst)
}

func (c *Client) finish(resp *http.Response, dst any) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if dst == nil || resp.StatusCode == http.StatusNoContent {
			io.Copy(io.Discard, resp.Body)
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(dst)
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e apiError
	_ = json.Unmarshal(raw, &e)
	if e.Code == discordErrCannotSendToUser {
		// Left the server, blocked us, or has DMs from server members off.
		// Discord doesn't distinguish, and the user-facing advice is the
		// same either way.
		return fmt.Errorf("%w: %s", ErrUndeliverable, e.Message)
	}
	if e.Message != "" {
		return fmt.Errorf("discord: status %d: %s (code %d)", resp.StatusCode, e.Message, e.Code)
	}
	return fmt.Errorf("discord: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

func retryAfter(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return time.Second
}
