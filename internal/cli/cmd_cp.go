package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// copySide is one end of `box cp`: a local path or a path inside a box.
type copySide struct {
	// BoxID is set when the path is inside a box.
	BoxID string
	// Path is the local path or the path in the guest.
	Path string
	// InBox reports whether BoxID is set.
	InBox bool
}

// parseCopySide reads "id:/path" or a plain local path.
func parseCopySide(spec string) copySide {
	if i := strings.Index(spec, ":"); i > 0 {
		id := spec[:i]
		if boxIDPattern.MatchString(id) && !strings.ContainsAny(id, "/\\") {
			// "id:/path" and also "id:" (the box root).
			rest := spec[i+1:]
			if rest == "" {
				rest = "."
			}
			return copySide{BoxID: id, Path: rest, InBox: true}
		}
	}
	return copySide{Path: spec}
}

// cpOutput is the JSON document of `box cp`.
type cpOutput struct {
	From  string   `json:"from"`
	To    string   `json:"to"`
	Box   string   `json:"box"`
	Files []string `json:"files,omitempty"`
	Bytes int64    `json:"bytes,omitempty"`
}

// cmdCP copies files and directories in and out of a box.
//
// The transfer is a tar stream over `nomad alloc exec` stdin/stdout, so it needs
// nothing but tar and a shell in the guest, and one connection for a whole tree.
func (a *App) cmdCP(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return usagef("box cp needs exactly two paths: box cp <src> <dst>, with id:/path on one side")
	}
	src, dst := parseCopySide(args[0]), parseCopySide(args[1])
	if src.InBox == dst.InBox {
		return usagef("box cp: exactly one side must be inside a box, for example `box cp ./dir dev1:/work/dir` or `box cp dev1:/work/out ./out`")
	}
	boxID := src.BoxID
	if !src.InBox {
		boxID = dst.BoxID
	}
	box, _, err := a.findBox(ctx, boxID)
	if err != nil {
		return err
	}
	box, err = a.waitRunning(ctx, box, guestWaitTimeout)
	if err != nil {
		return err
	}
	alloc, err := a.runningAlloc(ctx, box)
	if err != nil {
		return err
	}
	tr, err := a.guestTransport(ctx, alloc)
	if err != nil {
		return err
	}
	var out *cpOutput
	if src.InBox {
		out, err = a.copyFromBox(ctx, tr, box.ID, src.Path, dst.Path)
	} else {
		out, err = a.copyToBox(ctx, tr, box.ID, src.Path, dst.Path)
	}
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(out)
	}
	a.note("copied %s to %s (%s)", out.From, out.To, humanBytes(out.Bytes))
	return nil
}

// copyToBox streams a local file or directory into the box.
func (a *App) copyToBox(ctx context.Context, tr ExecTransport, boxID, localPath, guestPath string) (*cpOutput, error) {
	info, err := os.Lstat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, notFoundf("there is no local file or directory %s", localPath)
		}
		return nil, wrapf(err, "cannot read %s", localPath)
	}
	guestDir, entryName := splitGuestTarget(guestPath, filepath.Base(localPath))
	if err := guestMkdir(ctx, tr, guestDir); err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := PackTar(pw, localPath, entryName)
		pw.CloseWithError(err)
		packed <- err
	}()
	var stderr bytes.Buffer
	code, execErr := tr.Exec(ctx, ExecOpts{
		Command: []string{"tar", "-xf", "-", "-C", guestDir},
		Stdin:   pr,
		Stdout:  io.Discard,
		Stderr:  &stderr,
	})
	packErr := <-packed
	if packErr != nil {
		return nil, wrapf(packErr, "cannot read %s", localPath)
	}
	if execErr != nil {
		return nil, wrapf(execErr, "the copy into box %s failed", boxID)
	}
	if code != 0 {
		return nil, tarFailure(code, stderr.String(), fmt.Sprintf("box %s could not write %s", boxID, path.Join(guestDir, entryName)))
	}
	guestTarget := path.Join(guestDir, entryName)
	return &cpOutput{
		From:  localPath,
		To:    boxID + ":" + guestTarget,
		Box:   boxID,
		Files: []string{guestTarget},
		Bytes: treeBytes(localPath, info),
	}, nil
}

// copyFromBox streams a file or directory out of the box.
func (a *App) copyFromBox(ctx context.Context, tr ExecTransport, boxID, guestPath, localPath string) (*cpOutput, error) {
	guestDir, entryName := splitGuestSource(guestPath)
	destDir, targetName := splitLocalTarget(localPath, entryName, guestPath)

	pr, pw := io.Pipe()
	type execResult struct {
		code   int
		err    error
		stderr string
	}
	result := make(chan execResult, 1)
	var stderr bytes.Buffer
	go func() {
		code, err := tr.Exec(ctx, ExecOpts{
			Command: []string{"tar", "-cf", "-", "-C", guestDir, entryName},
			Stdout:  pw,
			Stderr:  &stderr,
		})
		pw.CloseWithError(err)
		result <- execResult{code: code, err: err, stderr: stderr.String()}
	}()

	names, unpackErr := UnpackTar(pr, destDir)
	// Unblock the guest side: if the unpack stopped early the stream would wait
	// for a reader that is gone.
	_ = pr.CloseWithError(unpackErr)
	res := <-result
	if unpackErr != nil {
		return nil, wrapf(unpackErr, "cannot write %s", localPath)
	}
	if res.err != nil {
		return nil, wrapf(res.err, "the copy out of box %s failed", boxID)
	}
	if res.code != 0 {
		return nil, tarFailure(res.code, res.stderr, fmt.Sprintf("box %s could not read %s", boxID, guestPath))
	}
	unpacked := entryName
	if len(names) == 1 {
		unpacked = names[0]
	}
	finalPath := filepath.Join(destDir, filepath.FromSlash(unpacked))
	if targetName != "" && targetName != unpacked {
		finalPath = filepath.Join(destDir, targetName)
		if err := os.RemoveAll(finalPath); err != nil {
			return nil, wrapf(err, "cannot replace %s", finalPath)
		}
		if err := os.Rename(filepath.Join(destDir, filepath.FromSlash(unpacked)), finalPath); err != nil {
			return nil, wrapf(err, "cannot rename the copy to %s", finalPath)
		}
	}
	info, _ := os.Lstat(finalPath)
	return &cpOutput{
		From:  boxID + ":" + guestPath,
		To:    finalPath,
		Box:   boxID,
		Files: names,
		Bytes: treeBytes(finalPath, info),
	}, nil
}

// splitGuestTarget splits a guest destination into the directory to extract in
// and the name of the entry.
func splitGuestTarget(guestPath, fallbackName string) (dir, name string) {
	if strings.HasSuffix(guestPath, "/") || guestPath == "." || guestPath == "" {
		dir = strings.TrimSuffix(guestPath, "/")
		if dir == "" {
			dir = "/"
		}
		return dir, fallbackName
	}
	return path.Dir(guestPath), path.Base(guestPath)
}

// splitGuestSource splits a guest source into its directory and its name.
func splitGuestSource(guestPath string) (dir, name string) {
	trimmed := strings.TrimSuffix(guestPath, "/")
	if trimmed == "" {
		trimmed = "/"
	}
	return path.Dir(trimmed), path.Base(trimmed)
}

// splitLocalTarget picks where an incoming entry is written.
func splitLocalTarget(localPath, entryName, guestPath string) (dir, name string) {
	if strings.HasSuffix(localPath, "/") || strings.HasSuffix(guestPath, "/") {
		return strings.TrimSuffix(localPath, "/"), entryName
	}
	if info, err := os.Stat(localPath); err == nil && info.IsDir() {
		return localPath, entryName
	}
	return filepath.Dir(localPath), filepath.Base(localPath)
}

// guestMkdir creates a directory in the guest.
func guestMkdir(ctx context.Context, tr ExecTransport, dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	var stderr bytes.Buffer
	code, err := tr.Exec(ctx, ExecOpts{
		Command: []string{"mkdir", "-p", "--", dir},
		Stdout:  io.Discard,
		Stderr:  &stderr,
	})
	if err != nil {
		return wrapf(err, "cannot create the directory %s in the box", dir)
	}
	if code != 0 {
		return errorf("cannot create the directory %s in the box: %s", dir, oneLine(stderr.String(), 200))
	}
	return nil
}

// tarFailure turns a failed tar exit into a plain-words error.
func tarFailure(code int, stderr, action string) error {
	detail := oneLine(stderr, 300)
	if detail == "" {
		detail = fmt.Sprintf("tar exited with code %d", code)
	}
	lower := strings.ToLower(detail)
	missing := strings.Contains(lower, "no such file") || strings.Contains(lower, "cannot stat") ||
		strings.Contains(lower, "cannot open") || strings.Contains(lower, "not found")
	if missing && !strings.Contains(lower, "permission denied") {
		return notFoundf("%s: %s", action, detail)
	}
	return errorf("%s: %s", action, detail)
}

// treeBytes returns the size of a file or of everything below a directory.
func treeBytes(path string, info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	if info.Mode().IsRegular() {
		return info.Size()
	}
	if !info.IsDir() {
		return 0
	}
	var total int64
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}
