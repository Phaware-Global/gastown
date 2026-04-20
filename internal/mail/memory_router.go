package mail

import "sync"

// MemoryRouter is an in-memory mail router for use in tests.
// It satisfies the same Send(*Message) error contract as *Router but
// never touches Dolt or the filesystem, making it safe to use in unit
// tests that are blocked by the test-send guard in Router.Send.
//
// Usage:
//
//	mr := mail.NewMemoryRouter()
//	// inject mr wherever a *mail.Router would normally go
//	// then inspect mr.Messages() after the call under test
type MemoryRouter struct {
	mu       sync.Mutex
	messages []*Message
}

// NewMemoryRouter returns an empty MemoryRouter.
func NewMemoryRouter() *MemoryRouter {
	return &MemoryRouter{}
}

// Send appends msg to the in-memory store. It never fails.
func (r *MemoryRouter) Send(msg *Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, msg)
	return nil
}

// Messages returns a snapshot of all messages received so far.
func (r *MemoryRouter) Messages() []*Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Message, len(r.messages))
	copy(out, r.messages)
	return out
}

// Reset clears all stored messages.
func (r *MemoryRouter) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = r.messages[:0]
}
