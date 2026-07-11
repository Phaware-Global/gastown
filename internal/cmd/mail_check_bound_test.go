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

	t.Run("urgent and high always kept, normal fills the rest", func(t *testing.T) {
		mixed := append(append(repeat(mail.PriorityUrgent, 5), repeat(mail.PriorityHigh, 3)...), repeat(mail.PriorityNormal, 100)...)
		b, om := boundInjectBatch(mixed, 40)
		if len(b) != 40 || om != 68 { // 5+3+32 shown, 108-40 omitted
			t.Errorf("got %d batch / %d omitted, want 40/68", len(b), om)
		}
		if countPri(b, mail.PriorityUrgent) != 5 || countPri(b, mail.PriorityHigh) != 3 {
			t.Errorf("dropped priority mail: %d urgent, %d high (want 5,3)", countPri(b, mail.PriorityUrgent), countPri(b, mail.PriorityHigh))
		}
	})

	t.Run("never drops urgent even beyond the limit", func(t *testing.T) {
		b, om := boundInjectBatch(repeat(mail.PriorityUrgent, 50), 40)
		if len(b) != 50 || om != 0 {
			t.Errorf("got %d batch / %d omitted, want 50/0 (urgent must never be dropped)", len(b), om)
		}
	})
}
