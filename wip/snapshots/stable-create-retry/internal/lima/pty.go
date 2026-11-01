package lima

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type TerminalSize struct{ Rows, Cols uint16 }

func quoteCommand(command []string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("empty command")
	}
	parts := make([]string, len(command))
	for i, v := range command {
		if strings.ContainsRune(v, 0) {
			return "", fmt.Errorf("NUL in command")
		}
		parts[i] = "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'"
	}
	return strings.Join(parts, " "), nil
}

// Uses only Lima-generated SSH identity/config; no personal keys or agent.
func (r CommandRunner) PTY(ctx context.Context, name string, command []string, in io.Reader, out io.Writer, sizes <-chan TerminalSize) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid instance")
	}
	shell, e := quoteCommand(command)
	if e != nil {
		return e
	}
	nonce := make([]byte, 16)
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	marker := []byte("NOMAD-LIMA-PTY-" + hex.EncodeToString(nonce) + "-READY")
	remote := "trap 'exit 130' INT; trap 'exit 143' TERM; printf '%s' '" + string(marker) + "'; " + shell + "; rc=$?; exit $rc"
	c := exec.CommandContext(ctx, "/usr/bin/ssh", "-F", filepath.Join(r.Home, name, "ssh.config"), "-o", "ForwardAgent=no", "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-tt", "lima-"+name, remote)
	c.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + r.UserHome, "TERM=xterm-256color"}
	c.SysProcAttr = &syscall.SysProcAttr{Credential: r.Credential}
	terminal, e := pty.StartWithSize(c, &pty.Winsize{Rows: 24, Cols: 80})
	if e != nil {
		return e
	}
	defer terminal.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case size, ok := <-sizes:
				if !ok {
					return
				}
				if size.Rows > 0 && size.Cols > 0 {
					_ = pty.Setsize(terminal, &pty.Winsize{Rows: size.Rows, Cols: size.Cols})
				}
			}
		}
	}()
	ready := make(chan struct{})
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		reader := bufio.NewReader(terminal)
		var prefix []byte
		for len(prefix) < 16384 {
			v, err := reader.ReadByte()
			if err != nil {
				return
			}
			prefix = append(prefix, v)
			if bytes.HasSuffix(prefix, marker) {
				_, _ = out.Write(prefix[:len(prefix)-len(marker)])
				close(ready)
				_, _ = io.Copy(out, reader)
				return
			}
		}
		_ = c.Process.Kill()
	}()
	if in != nil {
		if closer, ok := in.(io.Closer); ok {
			defer closer.Close()
		}
		go func() {
			select {
			case <-ready:
			case <-ctx.Done():
				return
			case <-done:
				return
			}
			_, _ = io.Copy(terminal, in)
			_, _ = terminal.Write([]byte{4})
		}()
	}
	e = c.Wait()
	select {
	case <-copied:
	case <-time.After(time.Second):
		terminal.Close()
		<-copied
	}
	select {
	case <-ready:
	default:
		return fmt.Errorf("SSH PTY readiness not confirmed; no retry")
	}
	return e
}
func (c *Client) PTY(ctx context.Context, name string, command []string, in io.Reader, out io.Writer, sizes <-chan TerminalSize) error {
	r, ok := c.Runner.(interface {
		PTY(context.Context, string, []string, io.Reader, io.Writer, <-chan TerminalSize) error
	})
	if !ok {
		return fmt.Errorf("PTY runner unavailable")
	}
	return r.PTY(ctx, name, command, in, out, sizes)
}
