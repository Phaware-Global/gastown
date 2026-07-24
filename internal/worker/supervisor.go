package worker

import (
	"context"
	"log/slog"
	"time"
)

// StopReason says why the supervisor ended the session.
type StopReason string

const (
	// StopInterrupted — the context was canceled: a provider interruption or
	// an operator shutdown signal (design §9.3).
	StopInterrupted StopReason = "interrupted"
	// StopMaxRuntime — the worker-side absolute runtime cap fired (§9.5).
	StopMaxRuntime StopReason = "max-runtime"
	// StopDeadman — contact with the control plane was lost for the
	// dead-man's-switch window (§9.5).
	StopDeadman StopReason = "deadman"
)

// SupervisorConfig configures the worker-side lifecycle supervisor.
type SupervisorConfig struct {
	// Checkpointer performs the §9.2 checkpoint/push cycle. Required.
	Checkpointer *Checkpointer

	// Interval is the checkpoint interval (a ceiling — busy worktrees skip a
	// tick via the quiescence guard). Required (> 0).
	Interval time.Duration

	// MaxRuntime is the worker-side absolute session cap (§9.5): reaching it
	// triggers StopWork + a final flush + self-release even if the host
	// reaper never calls. 0 disables the local cap.
	MaxRuntime time.Duration

	// DeadmanAfter is how long the control plane may be unreachable (no
	// successful push or probe) before the worker self-releases (§9.5's
	// dead-man's switch). 0 defaults to 4× Interval; negative disables.
	DeadmanAfter time.Duration

	// StopWork stops the agent's work process before the final flush
	// (§9.3 step 1): signal the native process, or `docker stop` the work
	// container. How is mode/provider-specific, so it is injected; nil is a
	// no-op (nothing supervised yet).
	StopWork func(ctx context.Context) error

	Log *slog.Logger
}

// Supervisor runs the worker-side lifecycle: the continuous checkpoint loop
// (§9.2), the shutdown sequence on interruption (§9.3), and the local
// max-runtime + dead-man self-release watchdog (§9.5).
type Supervisor struct {
	cfg SupervisorConfig
	log *slog.Logger
}

// NewSupervisor validates cfg and builds a Supervisor.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.DeadmanAfter == 0 {
		cfg.DeadmanAfter = 4 * cfg.Interval
	}
	return &Supervisor{cfg: cfg, log: cfg.Log}
}

// Run drives the lifecycle until the session ends, and returns why it ended.
// The shutdown sequence (§9.3) — StopWork, then a final quiescence-free
// checkpoint flush — runs for EVERY stop reason before Run returns; if the
// final flush cannot push, at most one interval of work is lost remotely
// while the local checkpoint ref still holds it.
//
// The caller decides what self-release means for its provider (§9.5) —
// terminate the instance, end the session — based on the returned reason.
func (s *Supervisor) Run(ctx context.Context) StopReason {
	start := time.Now()
	lastContact := time.Now()

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	var reason StopReason
loop:
	for {
		select {
		case <-ctx.Done():
			reason = StopInterrupted
			break loop
		case <-ticker.C:
		}

		// Watchdog checks first, so a wedged checkpoint path cannot starve
		// them (§9.5: these are the cost/safety backstop).
		if s.cfg.MaxRuntime > 0 && time.Since(start) >= s.cfg.MaxRuntime {
			s.log.Warn("worker max_runtime reached — self-releasing", "cap", s.cfg.MaxRuntime)
			reason = StopMaxRuntime
			break loop
		}
		if s.cfg.DeadmanAfter > 0 && time.Since(lastContact) >= s.cfg.DeadmanAfter {
			s.log.Warn("control plane unreachable past dead-man window — self-releasing",
				"window", s.cfg.DeadmanAfter, "lastContact", lastContact)
			reason = StopDeadman
			break loop
		}

		pushed, err := s.cfg.Checkpointer.Checkpoint(ctx)
		switch {
		case err != nil:
			// Push/remote failures feed the dead-man clock; everything keeps
			// running — a push outage delays durability, never blocks work
			// (§9.6).
			s.log.Warn("checkpoint failed", "err", err)
		case pushed:
			s.log.Debug("checkpoint pushed")
			lastContact = time.Now()
		default:
			// Nothing to push: probe so an idle-but-healthy session still
			// proves control-plane contact.
			if err := s.cfg.Checkpointer.Probe(ctx); err != nil {
				s.log.Warn("control-plane probe failed", "err", err)
			} else {
				lastContact = time.Now()
			}
		}
	}

	s.shutdown(reason)
	return reason
}

// shutdown is the §9.3 sequence: stop the work process, then flush the final
// delta to the checkpoint ref. Uses a fresh context — the run context is
// typically already canceled when we get here.
func (s *Supervisor) shutdown(reason StopReason) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if s.cfg.StopWork != nil {
		if err := s.cfg.StopWork(ctx); err != nil {
			s.log.Warn("stopping work process", "err", err)
		}
	}

	// Final flush ignores quiescence: capture whatever is there, now.
	saved := s.cfg.Checkpointer.Debounce
	s.cfg.Checkpointer.Debounce = 0
	defer func() { s.cfg.Checkpointer.Debounce = saved }()
	if pushed, err := s.cfg.Checkpointer.Checkpoint(ctx); err != nil {
		s.log.Warn("final checkpoint flush failed (local ref may still hold it)", "reason", reason, "err", err)
	} else if pushed {
		s.log.Info("final checkpoint flushed", "reason", reason)
	}
}
