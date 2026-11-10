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
//
// The copy runs as the box's login user, which is what `box cp` has always done,
// and what that user owns stays its own. A target it cannot write -- /work below
// the agent's own directories, which belong to boxd's user -- is written through
// `sudo -n` instead, and what is written belongs to the owner of the nearest
// directory that is already there: a copy into the agent's /work is still the
// agent's.
func (a *App) copyToBox(ctx context.Context, tr ExecTransport, boxID, localPath, guestPath string) (*cpOutput, error) {
	info, err := os.Lstat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, notFoundf("there is no local file or directory %s", localPath)
		}
		return nil, wrapf(err, "cannot read %s", localPath)
	}
	guestDir, entryName := splitGuestTarget(guestPath, filepath.Base(localPath))
	target, err := copyTargetIn(ctx, tr, guestDir)
	if err != nil {
		return nil, err
	}
	if !target.writable {
		if err := a.requireSudo(ctx, tr, fmt.Sprintf("writing %s in box %s", guestDir, boxID)); err != nil {
			return nil, err
		}
	}
	if err := guestMkdir(ctx, tr, guestDir, !target.writable); err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := PackTar(pw, localPath, entryName)
		pw.CloseWithError(err)
		packed <- err
	}()
	// An archive written as root keeps the uid it was packed with, which is the
	// local machine's and means nothing here: what is written is handed to the
	// target's owner below.
	command := []string{"tar", "-xf", "-", "-C", guestDir}
	if !target.writable {
		command = append([]string{"sudo", "-n", "tar", "-xf", "-", "--no-same-owner", "-C"}, guestDir)
	}
	var stderr bytes.Buffer
	code, execErr := tr.Exec(ctx, ExecOpts{
		Command: command,
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
	if !target.writable {
		if err := handOverCopy(ctx, tr, target, guestTarget); err != nil {
			return nil, err
		}
	}
	return &cpOutput{
		From:  localPath,
		To:    boxID + ":" + guestTarget,
		Box:   boxID,
		Files: []string{guestTarget},
		Bytes: treeBytes(localPath, info),
	}, nil
}

// copyFromBox streams a file or directory out of the box.
//
// It reads as the box's login user like the rest of `box cp`, and through
// `sudo -n` when that user cannot read the source -- what an agent made in /work
// is root's, and a result is read out of the box more often than it is written
// into it.
func (a *App) copyFromBox(ctx context.Context, tr ExecTransport, boxID, guestPath, localPath string) (*cpOutput, error) {
	guestDir, entryName := splitGuestSource(guestPath)
	destDir, targetName := splitLocalTarget(localPath, entryName, guestPath)
	readable, err := copyReadable(ctx, tr, path.Join(guestDir, entryName), guestDir)
	if err != nil {
		return nil, err
	}
	command := []string{"tar", "-cf", "-", "-C", guestDir, entryName}
	if !readable {
		if err := a.requireSudo(ctx, tr, fmt.Sprintf("reading %s in box %s", guestPath, boxID)); err != nil {
			return nil, err
		}
		command = append([]string{"sudo", "-n"}, command...)
	}

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
			Command: command,
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

// guestMkdir creates a directory in the guest, as the box's login user or, when
// that user may not write the place, through sudo -n.
func guestMkdir(ctx context.Context, tr ExecTransport, dir string, asRoot bool) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	command := []string{"mkdir", "-p", "--", dir}
	if asRoot {
		command = append([]string{"sudo", "-n"}, command...)
	}
	var stderr bytes.Buffer
	code, err := tr.Exec(ctx, ExecOpts{
		Command: command,
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

// copyTarget is where a copy into the box goes, and who may write it.
type copyTarget struct {
	// writable is whether the box's login user may write the target directory,
	// or the nearest directory above it that is already there.
	writable bool
	// owner is the uid:gid of that directory: what a copy written as root
	// belongs to afterwards.
	owner string
	// created are the directories a copy written as root makes, from the first
	// one that is missing down to the target.
	created []string
}

// copyTargetProbe reports who may write a directory in the box: the owner and the
// path of the nearest directory that is there, then whether the box's login user
// may write it (exit 0) or may not (exit 1). It is one call, so a copy into a
// place that is already the user's costs one answer and nothing else.
const copyTargetProbe = `d=$0; while [ ! -e "$d" ] && [ "$d" != / ]; do d=$(dirname -- "$d"); done; ` +
	`stat -c '%u:%g' -- "$d" && printf '%s\n' "$d"; [ -w "$d" ]`

// copyReadableProbe reports whether the box's login user may read what tar has to
// read for a copy out: the entry itself and the directory it is read from.
const copyReadableProbe = `[ -r "$0" ] && [ -x "$1" ]`

// copyTargetIn asks the guest where a copy into dir may be written, and who owns
// the place it would go.
func copyTargetIn(ctx context.Context, tr ExecTransport, dir string) (copyTarget, error) {
	var out, stderr bytes.Buffer
	code, err := tr.Exec(ctx, ExecOpts{
		Command: []string{"sh", "-c", copyTargetProbe, dir},
		Stdout:  &out,
		Stderr:  &stderr,
	})
	if err != nil {
		return copyTarget{}, wrapf(err, "cannot ask the box who may write %s", dir)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		return copyTarget{}, errorf("cannot ask the box who may write %s: %s", dir,
			oneLine(stderr.String(), 200))
	}
	return copyTarget{
		writable: code == 0,
		owner:    strings.TrimSpace(lines[0]),
		created:  createdDirs(strings.TrimSpace(lines[len(lines)-1]), dir),
	}, nil
}

// copyReadable reports whether the box's login user may read entry, which is
// inside dir.
func copyReadable(ctx context.Context, tr ExecTransport, entry, dir string) (bool, error) {
	var stderr bytes.Buffer
	code, err := tr.Exec(ctx, ExecOpts{
		Command: []string{"sh", "-c", copyReadableProbe, entry, dir},
		Stdout:  io.Discard,
		Stderr:  &stderr,
	})
	if err != nil {
		return false, wrapf(err, "cannot ask the box whether %s is readable", entry)
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, errorf("cannot ask the box whether %s is readable: %s", entry, oneLine(stderr.String(), 200))
}

// createdDirs are the directories between the nearest one that is there (parent)
// and the target: root makes them, so they are handed over with what is written.
func createdDirs(parent, dir string) []string {
	parent, dir = path.Clean(parent), path.Clean(dir)
	if parent == dir {
		return nil
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(dir, parent), "/")
	var dirs []string
	cur := parent
	for _, part := range strings.Split(rest, "/") {
		if part == "" {
			continue
		}
		cur = path.Join(cur, part)
		dirs = append(dirs, cur)
	}
	return dirs
}

// handOverCopy gives what a copy written as root made -- the directories and the
// entry -- to the owner of the nearest directory that was already there. A copy
// into the agent's /work is the agent's then, not root's.
func handOverCopy(ctx context.Context, tr ExecTransport, target copyTarget, entry string) error {
	paths := append(append([]string(nil), target.created...), entry)
	if target.owner == "" {
		return nil
	}
	var stderr bytes.Buffer
	code, err := tr.Exec(ctx, ExecOpts{
		Command: append([]string{"sudo", "-n", "chown", "-R", target.owner, "--"}, paths...),
		Stdout:  io.Discard,
		Stderr:  &stderr,
	})
	if err != nil {
		return wrapf(err, "cannot give %s in the box to %s", entry, target.owner)
	}
	if code != 0 {
		return errorf("%s in the box was written as root and could not be given to %s: %s",
			entry, target.owner, oneLine(stderr.String(), 200))
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
