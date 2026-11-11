package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// killGrace is how long a process group gets between SIGTERM and SIGKILL.
var killGrace = 10 * time.Second

// runProc runs a command in its own process group and streams stdout and
// stderr to out. When ctx ends, the whole group gets SIGTERM, then SIGKILL
// after killGrace, and ctx.Err() is returned. Processes the command left
// behind in its group are killed when it exits.
func runProc(ctx context.Context, out io.Writer, dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Hand the child a real pipe end rather than an io.Writer, so Wait returns
	// when the command exits even if a background child keeps the pipe open.
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	pw.Close()

	copied := make(chan struct{})
	go func() {
		io.Copy(out, pr)
		close(copied)
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	pgid := cmd.Process.Pid
	select {
	case err = <-waited:
	case <-ctx.Done():
		syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-waited:
		case <-time.After(killGrace):
			syscall.Kill(-pgid, syscall.SIGKILL)
			<-waited
		}
		err = ctx.Err()
	}
	syscall.Kill(-pgid, syscall.SIGKILL) // stragglers still in the group

	// Drain the remaining output. A process that left the group can hold the
	// pipe open forever, so don't wait for it indefinitely.
	select {
	case <-copied:
	case <-time.After(3 * time.Second):
	}
	pr.Close()
	<-copied
	return err
}

// runScript runs a build script: directly when it is executable and starts
// with a shebang (so the interpreter it names is used), otherwise through bash.
func runScript(ctx context.Context, out io.Writer, script, dir string, env []string) error {
	if hasShebang(script) {
		return runProc(ctx, out, dir, env, script)
	}
	return runProc(ctx, out, dir, env, "bash", script)
}

func hasShebang(script string) bool {
	st, err := os.Stat(script)
	if err != nil || st.Mode()&0o111 == 0 {
		return false
	}
	f, err := os.Open(script)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 2)
	_, err = io.ReadFull(f, head)
	return err == nil && string(head) == "#!"
}

// describeExit turns a process error into a short human message.
func describeExit(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return fmt.Sprintf("killed by signal %s", ws.Signal())
		}
		return fmt.Sprintf("exit code %d", ee.ExitCode())
	}
	return err.Error()
}

// logWriter writes a build log. Output is handled line by line so that secret
// values can be masked before anything reaches the file.
type logWriter struct {
	mu    sync.Mutex
	f     *os.File
	masks []string
	buf   []byte
}

func newLogWriter(path string, secrets []string) (*logWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, err
	}
	return &logWriter{f: f, masks: maskList(secrets)}, nil
}

// maskList prepares the strings to hide: whole values, and for multi-line
// values (keys, certificates) each line. Very short strings are skipped
// because masking them would shred ordinary output.
func maskList(secrets []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string, min int) {
		s = strings.TrimSpace(s)
		if len(s) >= min && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range secrets {
		if strings.ContainsAny(s, "\r\n") {
			for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
				add(line, 8)
			}
			continue
		}
		add(s, 4)
	}
	// Longest first, so a secret containing another is hidden whole.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j]) > len(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i+1])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 16<<10 { // a very long line: don't hold it back forever
		w.emit(w.buf)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

func (w *logWriter) emit(line []byte) {
	s := string(line)
	for _, m := range w.masks {
		s = strings.ReplaceAll(s, m, "***")
	}
	w.f.WriteString(s)
}

func (w *logWriter) flushPartial() {
	if len(w.buf) > 0 {
		w.emit(append(w.buf, '\n'))
		w.buf = w.buf[:0]
	}
}

// Notef writes a line from the service itself, marked so it stands apart from script output.
func (w *logWriter) Notef(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushPartial()
	w.emit([]byte(time.Now().UTC().Format("[builder 15:04:05 UTC] ") + fmt.Sprintf(format, args...) + "\n"))
}

func (w *logWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushPartial()
	w.f.Close()
}
