package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/mail"
)

func TestBoundInjectBatch(t *testing.T) {
	mk := func(p mail.Priority) *mail.Message { return &mail.Message{ID: "x", Priority: p} }
	repeat := func(p mail.Priority, n int) []*mail.Message {
		out := make([]*mail.Message, n)
		for i := range out {
			out[i] = mk(p)
		}
		return out
	}
	countPri := func(msgs []*mail.Message, p mail.Priority) int {
		n := 0
		for _, m := range msgs {
			if m.Priority == p {
				n++
			}
		}
		return n
	}

	t.Run("under limit returns all, none omitted", func(t *testing.T) {
		b, om := boundInjectBatch(repeat(mail.PriorityNormal, 5), 40)
		if len(b) != 5 || om != 0 {
			t.Fatalf("got %d batch / %d omitted, want 5/0", len(b), om)
		}
	})

	t.Run("all-normal over limit is capped", func(t *testing.T) {
		b, om := boundInjectBatch(repeat(mail.PriorityNormal, 100), 40)
		if len(b) != 40 || om != 60 {
			t.Errorf("got %d batch / %d omitted, want 40/60", len(b), om)
		}
	})

	t.Run("urgent kept; high then normal fill to the limit", func(t *testing.T) {
		mixed := append(append(repeat(mail.PriorityUrgent, 5), repeat(mail.PriorityHigh, 3)...), repeat(mail.PriorityNormal, 100)...)
		b, om := boundInjectBatch(mixed, 40)
		if len(b) != 40 || om != 68 { // 5 urgent + 3 high + 32 normal shown, 108-40 omitted
			t.Errorf("got %d batch / %d omitted, want 40/68", len(b), om)
		}
		if countPri(b, mail.PriorityUrgent) != 5 || countPri(b, mail.PriorityHigh) != 3 || countPri(b, mail.PriorityNormal) != 32 {
			t.Errorf("wrong fill: %d urgent, %d high, %d normal (want 5,3,32)",
				countPri(b, mail.PriorityUrgent), countPri(b, mail.PriorityHigh), countPri(b, mail.PriorityNormal))
		}
	})

	t.Run("high-priority flood is capped, urgent still all kept", func(t *testing.T) {
		// A telegraph flood marked high must not defeat the bound: 5 urgent + 100 high
		// + 50 normal at limit 40 → 5 urgent + 35 high (fill to 40), no normal.
		mixed := append(append(repeat(mail.PriorityUrgent, 5), repeat(mail.PriorityHigh, 100)...), repeat(mail.PriorityNormal, 50)...)
		b, om := boundInjectBatch(mixed, 40)
		if len(b) != 40 || om != 115 { // 155 total - 40 shown
			t.Errorf("got %d batch / %d omitted, want 40/115", len(b), om)
		}
		if countPri(b, mail.PriorityUrgent) != 5 || countPri(b, mail.PriorityHigh) != 35 || countPri(b, mail.PriorityNormal) != 0 {
			t.Errorf("flood not capped: %d urgent, %d high, %d normal (want 5,35,0)",
				countPri(b, mail.PriorityUrgent), countPri(b, mail.PriorityHigh), countPri(b, mail.PriorityNormal))
		}
	})

	t.Run("nil messages are dropped in every path", func(t *testing.T) {
		// Under the limit, with nils interspersed.
		msgs := []*mail.Message{nil, mk(mail.PriorityNormal), nil, mk(mail.PriorityHigh), nil}
		b, om := boundInjectBatch(msgs, 40)
		if len(b) != 2 || om != 0 {
			t.Fatalf("under-limit with nils: got %d batch / %d omitted, want 2/0", len(b), om)
		}
		for _, m := range b {
			if m == nil {
				t.Fatal("nil leaked into batch (under-limit path)")
			}
		}
		// Over the limit, with nils (100 non-nil normal → 40 shown, 60 omitted).
		over := append([]*mail.Message{nil, nil}, repeat(mail.PriorityNormal, 100)...)
		b, om = boundInjectBatch(over, 40)
		if len(b) != 40 || om != 60 {
			t.Errorf("over-limit with nils: got %d batch / %d omitted, want 40/60", len(b), om)
		}
		for _, m := range b {
			if m == nil {
				t.Fatal("nil leaked into batch (over-limit path)")
			}
		}
	})

	t.Run("never drops urgent even beyond the limit", func(t *testing.T) {
		b, om := boundInjectBatch(repeat(mail.PriorityUrgent, 50), 40)
		if len(b) != 50 || om != 0 {
			t.Errorf("got %d batch / %d omitted, want 50/0 (urgent must never be dropped)", len(b), om)
		}
	})
}
