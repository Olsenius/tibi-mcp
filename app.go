package main

import (
	"context"
	"errors"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errNotConfigured = errors.New("the Tibi account is not set up yet: set the borrower number and PIN on the gateway's settings page, or TIBI_CARD_NUMBER and TIBI_PIN")

// app holds the live configuration. Saving settings through POST /config calls
// reload, which swaps the Tibi client and adds or removes the write tools
// without a restart.
type app struct {
	store  *store
	env    Settings
	envSet map[string]bool

	mu       sync.RWMutex
	settings Settings
	tibi     *client
	// newClient is replaced in tests.
	newClient func(card, pin string) *client

	server  *mcp.Server
	toolMu  sync.Mutex
	writeOn bool
}

func newApp(st *store) (*app, error) {
	env, set, err := fromEnv()
	if err != nil {
		return nil, err
	}
	a := &app{store: st, env: env, envSet: set, newClient: newClient}
	a.server = newServer(a, true)
	a.reload()
	return a, nil
}

// reload rebuilds the client from stored settings plus the environment. Login
// cookies are dropped only when the account changes.
func (a *app) reload() {
	s := merge(a.store.settings(), a.env, a.envSet)
	a.mu.Lock()
	old := a.settings
	if s.CardNumber != old.CardNumber || s.PIN != old.PIN || a.tibi == nil {
		a.tibi = nil
		if s.ready() {
			a.tibi = a.newClient(s.CardNumber, s.PIN)
		}
	}
	a.settings = s
	a.mu.Unlock()

	a.toolMu.Lock()
	defer a.toolMu.Unlock()
	if s.readOnly() && a.writeOn {
		a.server.RemoveTools(writeTools...)
		a.writeOn = false
	} else if !s.readOnly() && !a.writeOn {
		addWriteTools(a.server, a)
		a.writeOn = true
	}
}

func (a *app) current() Settings {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.settings
}

func (a *app) client() (*client, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.tibi == nil {
		return nil, errNotConfigured
	}
	return a.tibi, nil
}

// The tibiAPI methods use the current client. Searching works without an
// account.

func (a *app) Search(ctx context.Context, q string, page int) (any, error) {
	c, err := a.client()
	if err != nil {
		c = newClient("", "")
	}
	return c.Search(ctx, q, page)
}

func (a *app) Book(ctx context.Context, id string) (any, error) {
	c, err := a.client()
	if err != nil {
		c = newClient("", "")
	}
	return c.Book(ctx, id)
}

func (a *app) call(fn func(*client) (any, error)) (any, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	return fn(c)
}

func (a *app) Borrow(ctx context.Context, id string) (any, error) {
	return a.call(func(c *client) (any, error) { return c.Borrow(ctx, id) })
}

func (a *app) SendKindle(ctx context.Context, id string) (any, error) {
	return a.call(func(c *client) (any, error) { return c.SendKindle(ctx, id) })
}

func (a *app) EPUBLink(ctx context.Context, id string) (any, error) {
	return a.call(func(c *client) (any, error) { return c.EPUBLink(ctx, id) })
}

func (a *app) KindleEmail(ctx context.Context) (any, error) {
	return a.call(func(c *client) (any, error) { return c.KindleEmail(ctx) })
}

func (a *app) SetKindleEmail(ctx context.Context, addr string) (any, error) {
	return a.call(func(c *client) (any, error) { return c.SetKindleEmail(ctx, addr) })
}
