package mcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stdioTransport starts a server as the leader of its own process group and
// talks to it over its stdin and stdout. The SDK's CommandTransport signals
// only the process it started, so a server run through a wrapper such as
// npx or a shell would leave the real server running.
type stdioTransport struct {
	server Server
	dir    string
	grace  time.Duration
	logger *slog.Logger
	// proc is set once Connect has started the process.
	proc *process
}

// Connect starts the server. The context is not used: the process lives
// until the connection is closed.
func (t *stdioTransport) Connect(context.Context) (sdk.Connection, error) {
	cmd := exec.Command(t.server.Command, t.server.Args...)
	cmd.Dir = t.dir
	cmd.Env = append(os.Environ(), t.server.Env...)
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", t.server.Command, err)
	}
	p := &process{cmd: cmd, stdin: stdin, stdout: stdout, grace: t.grace, exited: make(chan struct{}), gone: make(chan struct{})}
	p.reader = &watchedReader{p: p}
	t.proc = p
	var stderrDone sync.WaitGroup
	stderrDone.Go(func() {
		// A line longer than the scanner's buffer ends the scan, and the
		// rest is discarded so that the server does not block on a full
		// pipe.
		lines := bufio.NewScanner(stderr)
		for lines.Scan() {
			t.logger.Debug("mcp: server stderr", "server", t.server.Name, "line", lines.Text())
		}
		_, _ = io.Copy(io.Discard, stderr)
	})
	go func() {
		// A child of the server that outlives it keeps the stderr pipe
		// open. The group is killed when the leader is gone, which closes
		// it, so this wait ends.
		<-p.gone
		stderrDone.Wait()
		// The exit status says nothing useful: a server killed here exits
		// with a signal.
		_ = cmd.Wait()
		close(p.exited)
	}()
	transport := &sdk.IOTransport{Reader: p.reader, Writer: closer{Writer: stdin, close: p.shutdown}}
	return transport.Connect(context.Background())
}

// process is a running stdio server.
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	grace  time.Duration
	reader io.ReadCloser
	// gone is closed when stdout has ended and the group has been killed.
	// exec's Wait must not run while stdout is still being read.
	gone chan struct{}
	// exited is closed when the process has been reaped.
	exited    chan struct{}
	once      sync.Once
	eofOnce   sync.Once
	drainOnce sync.Once
}

// eof runs once, when a read of stdout first fails. End of stdout means the
// server exited or closed it. Either way the connection is over, so nothing
// of the group may stay.
func (p *process) eof() {
	p.eofOnce.Do(func() {
		p.killGroup()
		close(p.gone)
	})
}

// drain reads stdout to its end once the SDK has stopped reading it, so
// that the end is seen and the process is reaped.
func (p *process) drain() {
	p.drainOnce.Do(func() {
		go func() {
			_, _ = io.Copy(io.Discard, p.stdout)
			p.eof()
		}()
	})
}

// shutdown ends the server the way the MCP specification asks: close its
// stdin, wait, SIGTERM, wait, SIGKILL. The signals go to the whole group.
func (p *process) shutdown() error {
	p.once.Do(func() {
		_ = p.stdin.Close()
		if p.wait() {
			return
		}
		_ = signalGroup(p.cmd.Process.Pid, syscall.SIGTERM)
		if p.wait() {
			return
		}
		p.killGroup()
	})
	return nil
}

func (p *process) wait() bool {
	timer := time.NewTimer(p.grace)
	defer timer.Stop()
	select {
	case <-p.exited:
		return true
	case <-timer.C:
		return false
	}
}

// killGroup kills the server and every process still in its group. A group
// that is already gone is not an error.
func (p *process) killGroup() {
	_ = signalGroup(p.cmd.Process.Pid, syscall.SIGKILL)
}

// stop kills whatever is left of the group and waits for the server to be
// reaped.
func (p *process) stop() {
	p.killGroup()
	p.drain()
	<-p.exited
}

// watchedReader is the server's stdout as the SDK reads it.
type watchedReader struct{ p *process }

func (w *watchedReader) Read(b []byte) (int, error) {
	n, err := w.p.stdout.Read(b)
	if err != nil {
		w.p.eof()
	}
	return n, err
}

// Close does not close the pipe: a server that is still writing its last
// answer would get SIGPIPE. The pipe closes when the process is reaped.
func (w *watchedReader) Close() error {
	w.p.drain()
	return nil
}

// closer is a writer whose Close runs a function.
type closer struct {
	io.Writer
	close func() error
}

func (c closer) Close() error { return c.close() }
