package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type Fingerprint struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type Prepared struct {
	Program      string        `json:"program"`
	Dir          string        `json:"dir"`
	Args         []string      `json:"args"`
	Fingerprints []Fingerprint `json:"fingerprints"`
}

type Execution struct {
	Started bool
	Exited  bool
	Output  []byte
}

func Prepare(ctx context.Context, program string, args, files []string) (Prepared, error) {
	if !validProgram(program, args, files, 0) {
		return Prepared{}, &Error{Code: "invalid_definition"}
	}
	dir, err := os.Getwd()
	if err != nil {
		return Prepared{}, &Error{Code: "program_unavailable"}
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return Prepared{}, &Error{Code: "program_unavailable"}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Prepared{}, &Error{Code: "program_unavailable"}
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return Prepared{}, &Error{Code: "program_unavailable"}
	}
	p := Prepared{Program: path, Dir: dir, Args: append([]string{}, args...), Fingerprints: []Fingerprint{}}
	seen := map[string]bool{}
	for _, name := range append([]string{path}, files...) {
		absolute, err := filepath.Abs(name)
		if err != nil {
			return Prepared{}, &Error{Code: "program_unavailable"}
		}
		if seen[absolute] {
			continue
		}
		sum, err := fileDigest(ctx, absolute)
		if err != nil {
			return Prepared{}, &Error{Code: "program_unavailable"}
		}
		p.Fingerprints = append(p.Fingerprints, Fingerprint{Path: absolute, Digest: sum})
		seen[absolute] = true
	}
	return p, nil
}

func fileDigest(ctx context.Context, path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() {
		return "", &Error{Code: "program_unavailable"}
	}
	h := sha256.New()
	if _, err = io.Copy(h, digestReader{ctx: ctx, reader: f}); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (p Prepared) verify(ctx context.Context) error {
	if p.Program == "" || !filepath.IsAbs(p.Program) || !filepath.IsAbs(p.Dir) || len(p.Fingerprints) == 0 ||
		p.Fingerprints[0].Path != p.Program || !validProgram(p.Program, p.Args, nil, 0) {
		return &Error{Code: "program_changed"}
	}
	for _, fp := range p.Fingerprints {
		sum, err := fileDigest(ctx, fp.Path)
		if ctx.Err() != nil {
			return &Error{Code: "cancelled"}
		}
		if err != nil || sum != fp.Digest {
			return &Error{Code: "program_changed"}
		}
	}
	return nil
}

func Run(ctx context.Context, p Prepared, input []byte) (Execution, error) {
	r := Execution{}
	if len(input) > MaxOutput {
		return r, &Error{Code: "input_limit"}
	}
	if err := p.verify(ctx); err != nil {
		return r, err
	}
	if ctx.Err() != nil {
		return r, &Error{Code: "cancelled"}
	}
	cmd := exec.CommandContext(ctx, p.Program, p.Args...)
	cmd.Dir = p.Dir
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	out := &limited{}
	cmd.Stdout = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return r, &Error{Code: "start_failed"}
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	r.Started = true
	err := cmd.Wait()
	r.Exited = cmd.ProcessState != nil && cmd.ProcessState.Exited()
	if ctx.Err() != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return r, &Error{Code: "timeout"}
		}
		return r, &Error{Code: "cancelled"}
	}
	if out.overflow {
		return r, &Error{Code: "output_limit"}
	}
	if err != nil {
		return r, &Error{Code: "exit_failed"}
	}
	r.Output = append([]byte(nil), out.data...)
	return r, nil
}

type limited struct {
	data     []byte
	overflow bool
}

type digestReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r digestReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (b *limited) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, MaxOutput-len(b.data))
	b.data = append(b.data, p[:keep]...)
	if keep < n {
		b.overflow = true
	}
	return n, nil
}
