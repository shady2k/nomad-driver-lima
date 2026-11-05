package cli

// A box is its volume and its disk, not its job: Nomad's job garbage collector
// deletes a dead job after `job_gc_threshold` (4 hours by default), and with it
// the only record of how the box was made. A box stopped at 02:16 answered "no
// such box" at 07:13 with its disk intact, while `box stop` had promised that
// `box start` brings it back.
//
// The rendered job is therefore kept in a Nomad Variable, where GC does not
// reach: nomad/jobs/box-<id>/spec. It holds the job and nothing else, so it holds
// no secret -- a credential reaches a box through a template that names a Vault
// path or reads the executors variable at render time, never through a value in
// the job (see executorTemplates).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

const (
	// boxSpecVariablePrefix holds the description of every box: the variable of
	// a box is boxSpecVariablePrefix+box-<id>+"/spec".
	boxSpecVariablePrefix = "nomad/jobs/"
	// boxSpecItem is the item of the variable that holds the description.
	boxSpecItem = "spec"
	// boxSpecSchema is the version of the document stored in that item.
	boxSpecSchema = 1
	// boxSpecVariableLimit is what Nomad accepts for one variable in total
	// (maxVariableSize in nomad/structs/variables.go). A description that does
	// not fit is refused before anything is created, with a sentence.
	boxSpecVariableLimit = 64 << 10
)

// BoxSpecVariable returns the Nomad Variable path that holds the description of
// a box.
func BoxSpecVariable(id string) string {
	return boxSpecVariablePrefix + BoxJobName(id) + "/" + boxSpecItem
}

// boxSpecDocument is the item a box's description is stored in.
type boxSpecDocument struct {
	Schema int `json:"schema"`
	// WrittenAt is when the description was written, so `box status` can say
	// how old it is.
	WrittenAt string `json:"written_at,omitempty"`
	// Job is the rendered job: what `box start` registers again when Nomad no
	// longer has it.
	Job *api.Job `json:"job"`
}

// encodeBoxSpec renders the description of a box: the job, as JSON.
func encodeBoxSpec(job *api.Job, now time.Time) ([]byte, error) {
	if job == nil {
		return nil, errorf("a box description needs a job")
	}
	doc := boxSpecDocument{Schema: boxSpecSchema, WrittenAt: now.UTC().Format(time.RFC3339), Job: job}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, wrapf(err, "the job of the box cannot be written as JSON")
	}
	return data, nil
}

// requireBoxSpecFits refuses a description that Nomad would not store.
//
// It runs before the disk is created, so a job too large to describe leaves no
// half box behind: the operator hears one sentence and nothing is changed.
func requireBoxSpecFits(id string, doc []byte) error {
	if len(doc) <= boxSpecVariableLimit {
		return nil
	}
	return errorf("the job of box %s is %d bytes as JSON, and a Nomad variable holds at most %d: "+
		"the box was not created. A very large executor configuration is what makes a job big "+
		"(`box executors show` lists the files); `box stop %s` and `box start %s` refresh the "+
		"executor files of a box without storing a bigger job", id, len(doc), boxSpecVariableLimit, id, id)
}

// writeBoxSpec stores the description of a box.
func (a *App) writeBoxSpec(ctx context.Context, id string, doc []byte) error {
	path := BoxSpecVariable(id)
	if _, err := a.Nomad.VariableWrite(ctx, &api.Variable{
		Path:  path,
		Items: map[string]string{boxSpecItem: string(doc)},
	}); err != nil {
		return wrapf(err, "Nomad could not store the description of box %s in the variable %s%s",
			id, path, variableDenied(err, path))
	}
	return nil
}

// storeBoxSpec renders and stores the description of a box.
func (a *App) storeBoxSpec(ctx context.Context, id string, job *api.Job) error {
	doc, err := encodeBoxSpec(job, a.now())
	if err != nil {
		return err
	}
	if err := requireBoxSpecFits(id, doc); err != nil {
		return err
	}
	return a.writeBoxSpec(ctx, id, doc)
}

// storedBoxJob returns the job a box's description holds, or nil when the box
// has no description. A description that cannot be read is an error: a missing
// one and an unreadable one are different facts, and only the first means "a box
// made before descriptions existed".
func (a *App) storedBoxJob(ctx context.Context, id string) (*api.Job, boxSpecDocument, error) {
	path := BoxSpecVariable(id)
	v, err := a.Nomad.VariableRead(ctx, path)
	if err != nil {
		if isNotFoundErr(err) {
			return nil, boxSpecDocument{}, nil
		}
		return nil, boxSpecDocument{}, wrapf(err, "cannot read the description of box %s from the variable %s%s",
			id, path, variableDenied(err, path))
	}
	if v == nil {
		return nil, boxSpecDocument{}, nil
	}
	text, ok := v.Items[boxSpecItem]
	if !ok || strings.TrimSpace(text) == "" {
		return nil, boxSpecDocument{}, errorf("the variable %s holds no %q item, so box %s has no description: "+
			"`box rm %s --yes` deletes the disk, and `box create %s ...` makes a new box",
			path, boxSpecItem, id, id, id)
	}
	var doc boxSpecDocument
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, boxSpecDocument{}, wrapf(err, "the description of box %s in %s is not readable JSON", id, path)
	}
	if doc.Schema != boxSpecSchema {
		return nil, boxSpecDocument{}, errorf("the description of box %s in %s has schema %d, and this command "+
			"reads schema %d: upgrade `box` to read it", id, path, doc.Schema, boxSpecSchema)
	}
	if doc.Job == nil {
		return nil, boxSpecDocument{}, errorf("the description of box %s in %s holds no job", id, path)
	}
	return doc.Job, doc, nil
}

// forgetBoxSpec removes the description of a box, and reports whether there was
// one. A description that is already gone is not an error: `box rm` is
// idempotent about the two records of a box.
func (a *App) forgetBoxSpec(ctx context.Context, id string) (bool, error) {
	path := BoxSpecVariable(id)
	if err := a.Nomad.VariableDelete(ctx, path); err != nil {
		if isNotFoundErr(err) {
			return false, nil
		}
		return false, wrapf(err, "Nomad could not delete the variable %s%s", path, variableDenied(err, path))
	}
	return true, nil
}

// variableDenied adds to a refused variable access what the token lacks. Nomad
// answers a bare "Permission denied" and never names the missing rule, and a
// token that may run every box command of an older cluster has no rule for the
// path a box's description lives on.
func variableDenied(err error, path string) string {
	if !isPermissionDeniedErr(err) {
		return ""
	}
	return fmt.Sprintf(" -- the Nomad token needs the variables capabilities read, write and destroy on %q "+
		"(or on \"nomad/jobs/box-*\"); deploy/acl/box-operator.policy.hcl grants them", path)
}
