//go:build windows

package workerclient

import (
	"errors"
	"os/exec"

	"github.com/steveyegge/gastown/internal/sockproto"
)

// errNoPTY is what every terminal operation reports on windows. The pty path
// rests on unix primitives with no windows equivalent — a controlling terminal
// via Setsid/Setctty, termios line-discipline control, and the TIOCSWINSZ
// resize ioctl — so this platform refuses a TTY attach outright rather than
// handing the launcher a terminal that does not behave like one.
//
// The worker client targets unix hosts; this file exists so the package builds
// and is smoke-tested cross-platform. A launcher that asks for a TTY here gets
// a clean error, and a non-TTY attach is unaffected.
var errNoPTY = errors.New("pty allocation is not supported on windows")

type ptySession struct{}

func startPTY(*exec.Cmd, *sockproto.Resize, bool) (*ptySession, error) { return nil, errNoPTY }

func (p *ptySession) Read([]byte) (int, error)      { return 0, errNoPTY }
func (p *ptySession) Write([]byte) (int, error)     { return 0, errNoPTY }
func (p *ptySession) Close() error                  { return nil }
func (p *ptySession) resize(sockproto.Resize) error { return errNoPTY }
