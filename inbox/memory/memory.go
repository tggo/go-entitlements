// Package memory is an in-process inbox.Inbox for tests and demos.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/tggo/go-entitlements"
	"github.com/tggo/go-entitlements/inbox"
)

// Inbox keeps everything in a map behind one mutex. It is safe for concurrent use.
type Inbox struct {
	mu    sync.Mutex
	items map[string]inbox.Stored
}

// New returns an empty Inbox.
func New() *Inbox { return &Inbox{items: map[string]inbox.Stored{}} }

var _ inbox.Inbox = (*Inbox)(nil)

func clone(s inbox.Stored) inbox.Stored {
	s.Body = slices.Clone(s.Body)
	s.Headers = inbox.SafeHeaders(s.Headers) // already safe; this copies the map and slices
	return s
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("inbox: generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Save implements inbox.Inbox.
func (m *Inbox) Save(ctx context.Context, raw inbox.Raw) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := inbox.Validate(raw); err != nil {
		return "", err
	}
	id := raw.ID
	if id == "" {
		var err error
		if id, err = newID(); err != nil {
			return "", err
		}
	}
	now := time.Now().UTC()
	if raw.ReceivedAt.IsZero() {
		raw.ReceivedAt = now
	}
	if raw.Route == "" {
		raw.Route = inbox.RouteProduction
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.items[id]; exists {
		return id, nil
	}
	raw.ID = id
	raw.ReceivedAt = raw.ReceivedAt.UTC()
	m.items[id] = clone(inbox.Stored{Raw: raw, ID: id, Status: inbox.StatusReceived, UpdatedAt: now})
	return id, nil
}

// Get implements inbox.Inbox.
func (m *Inbox) Get(ctx context.Context, id string) (*inbox.Stored, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.items[id]
	if !ok {
		return nil, entitlements.ErrNotFound
	}
	c := clone(s)
	return &c, nil
}

// Mark implements inbox.Inbox.
func (m *Inbox) Mark(ctx context.Context, id string, status inbox.Status, errText string, attempts int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !status.Valid() {
		return fmt.Errorf("%w: status %q", inbox.ErrInvalid, status)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.items[id]
	if !ok {
		return entitlements.ErrNotFound
	}
	s.Status, s.Error, s.Attempts, s.UpdatedAt = status, errText, attempts, time.Now().UTC()
	m.items[id] = s
	return nil
}

func less(a, b inbox.Stored) int {
	if c := a.ReceivedAt.Compare(b.ReceivedAt); c != 0 {
		return c
	}
	switch {
	case a.ID < b.ID:
		return -1
	case a.ID > b.ID:
		return 1
	}
	return 0
}

// List implements inbox.Inbox.
func (m *Inbox) List(ctx context.Context, f inbox.Filter) ([]inbox.Stored, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = inbox.DefaultListLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var cursor *inbox.Stored
	if f.After != "" {
		c, ok := m.items[f.After]
		if !ok {
			return nil, entitlements.ErrNotFound
		}
		cursor = &c
	}
	var out []inbox.Stored
	for _, s := range m.items {
		switch {
		case f.Status != "" && s.Status != f.Status,
			f.Source != "" && s.Source != f.Source,
			!f.Since.IsZero() && s.ReceivedAt.Before(f.Since),
			cursor != nil && less(s, *cursor) <= 0:
			continue
		}
		out = append(out, s)
	}
	slices.SortFunc(out, less)
	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i] = clone(out[i])
	}
	return out, nil
}

// Prune implements inbox.Inbox.
func (m *Inbox) Prune(ctx context.Context, before time.Time) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, s := range m.items {
		if s.Status.Final() && s.ReceivedAt.Before(before) {
			delete(m.items, id)
			n++
		}
	}
	return n, nil
}
