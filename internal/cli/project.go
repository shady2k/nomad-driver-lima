package cli

// A box carries the project it belongs to, in the job's meta key `project` and so
// in the description stored for it: `box ls` and `box capacity` show it, and the
// hints a command prints name the boxes of the caller's own project.
//
// It is a label, not a protection. Nothing is refused or stopped because of it,
// and `box ls --project` is a view of it and nothing more: the label is what lets
// one agent tell its boxes from another's at the moment it decides whether to stop
// one.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// boxProjectEnv is the variable that names the project of a box when --project is
// not given.
const boxProjectEnv = "BOX_PROJECT"

// boxProjectPattern is what a project label may look like: lower-case letters,
// digits and dashes.
var boxProjectPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// sanitizeProject turns a name that was not given as a label -- the base name of
// the repository of the current directory -- into one: lower-case letters, digits
// and single dashes, with nothing at either end.
func sanitizeProject(name string) string {
	var b strings.Builder
	dash := true // a leading dash is trimmed, like a trailing one
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteRune('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// validateProject checks a project label that was given by a person: --project,
// or BOX_PROJECT. Both are refused in a sentence rather than sanitized, because a
// label that does not say what the person meant is worse than a refusal.
func validateProject(value, source string) (string, error) {
	label := strings.TrimSpace(value)
	if label == "" {
		return "", nil
	}
	if !boxProjectPattern.MatchString(label) {
		if source == "flag" {
			return "", usagef("--project %q is not usable: use lower-case letters, digits and dashes, "+
				"for example my-project", label)
		}
		return "", usagef("%s is %q, which is not usable: use lower-case letters, digits and dashes, "+
			"for example my-project (or unset it)", boxProjectEnv, label)
	}
	return label, nil
}

// ProjectLabel returns the project a box created from this command belongs to:
// --project when it is given, else BOX_PROJECT, else the base name of the git
// repository the command runs in, and "" when none of them says.
//
// The label of a directory comes from `git rev-parse --git-common-dir`, so a
// linked worktree carries the name of the main repository: the two are one
// project.
func (a *App) ProjectLabel(value string, given bool) (string, error) {
	if given {
		return validateProject(value, "flag")
	}
	if env := strings.TrimSpace(a.getenv(boxProjectEnv)); env != "" {
		return validateProject(env, "env")
	}
	return a.repoProject(a.projectDir()), nil
}

// projectDir is the directory the repository name is read from.
func (a *App) projectDir() string {
	if a.ProjectDir != "" {
		return a.ProjectDir
	}
	if dir, err := os.Getwd(); err == nil {
		return dir
	}
	return ""
}

// lsProject turns the filters of `box ls` into the one project to show, or ""
// when every box is to be listed.
func (a *App) lsProject(value string, mine, given bool) (string, error) {
	if mine && given {
		return "", usagef("box ls: --project and --mine ask for one project each; give one of them")
	}
	if given {
		label := strings.TrimSpace(value)
		if label == "" {
			return "", usagef("--project needs a name: lower-case letters, digits and dashes, for example my-project")
		}
		return validateProject(label, "flag")
	}
	if !mine {
		return "", nil
	}
	label, err := a.ProjectLabel("", false)
	if err != nil {
		return "", err
	}
	if label == "" {
		return "", usagef("box ls --mine: this directory belongs to no project -- it is not in a git repository, " +
			"and BOX_PROJECT is not set; name one with --project NAME")
	}
	return label, nil
}

// boxesOfProject keeps the boxes of one project. A box with no label belongs to
// no project, so it is never one of them, and nothing fails for it.
func boxesOfProject(boxes []BoxRecord, project string) []BoxRecord {
	out := make([]BoxRecord, 0, len(boxes))
	for _, box := range boxes {
		if box.Project == project {
			out = append(out, box)
		}
	}
	return out
}

// repoProject is the project a directory belongs to: the name of the directory
// that holds its common git directory.
func (a *App) repoProject(dir string) string {
	if a.repoName != nil {
		return sanitizeProject(a.repoName(dir))
	}
	return gitRepoName(dir)
}

// getenv reads the process environment, unless a test set its own reader.
func (a *App) getenv(key string) string {
	if a.Getenv != nil {
		return a.Getenv(key)
	}
	return os.Getenv(key)
}

// gitRepoName returns the name of the git repository dir belongs to, or "" when
// it belongs to none.
//
// The common git directory is what is asked for, not the top level: in a linked
// worktree `--show-toplevel` names the worktree's own directory, while
// `--git-common-dir` names the main repository's `.git`, whose parent is the
// repository. A worktree is not a second project.
func gitRepoName(dir string) string {
	if dir == "" {
		dir = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	common = filepath.Clean(common)
	name := filepath.Base(common)
	if name == ".git" {
		name = filepath.Base(filepath.Dir(common))
	}
	if name == "." || name == string(filepath.Separator) {
		return ""
	}
	return name
}
