package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// boxStoreVariable is the Nomad Variable that holds the operator's image store:
// where it is, and the token that may write to it.
//
// It is its own variable, not part of the executor credentials, for two reasons: a
// box never needs it (a box downloads an image, it never publishes one), and a
// publish job is a sibling of it, so only a publish job's workload policy grants
// the read (deploy/acl/box-store.policy.hcl).
//
// The URL and the token are rendered into the publish task's secrets by templates,
// so the job spec holds the item names and nothing else. The token may also be a
// template that reads it from Vault at render time, which is the form to prefer:
// then even the Variable holds a path.
const boxStoreVariable = "nomad/jobs/box-store"

// storeOutput is the JSON document of `box store show` and `box store set`.
type storeOutput struct {
	// URL is the store's base URL.
	URL string `json:"url,omitempty"`
	// Token says how the token reaches a publish task: "variable" (a plain item),
	// "template" (a template that reads it at render time) or "" for a store that
	// needs none. It never says the token.
	Token string `json:"token,omitempty"`
	// Items are the item names of the variable, never their values.
	Items []string `json:"items,omitempty"`
	// Configured is false when no store is set.
	Configured bool `json:"configured"`
}

// cmdStore reads and writes the operator's image store.
func (a *App) cmdStore(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("box store needs a subcommand: set (--url URL with --from-file FILE or --template FILE), or show")
	}
	switch args[0] {
	case "set":
		return a.cmdStoreSet(ctx, args[1:])
	case "show":
		return a.cmdStoreShow(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintf(a.ErrOut, "usage: box store set --url URL (--from-file FILE | --template FILE)\n"+
			"       box store show\n\n"+
			"The store is the Nomad variable %s: where an image is published, and the\n"+
			"token that may write there. A publish job reads it through its own templates,\n"+
			"so neither the job spec nor this command ever holds the token in an argument.\n\n"+
			"--from-file stores the token in the Variable; --template stores Nomad template\n"+
			"text that reads it at render time, for example\n"+
			`  {{ with secret "kv/data/box/store" }}{{ .Data.data.token }}{{ end }}`+"\n"+
			"which keeps even the Variable free of the value. `box store show` prints the url\n"+
			"and the item names, never a value.\n", boxStoreVariable)
		return nil
	}
	return usagef("unknown store subcommand %q: use set or show", args[0])
}

// cmdStoreSet writes the Variable a publish job reads its store from.
func (a *App) cmdStoreSet(ctx context.Context, args []string) error {
	fs := newFlagSet("store set")
	url := fs.String("url", "", "the base URL of the store; the box-base/ layout is written below it")
	fromFile := fs.String("from-file", "", "a file holding the store's write token (stored in the Variable)")
	template := fs.String("template", "", "a file holding Nomad template text that reads the token at render time, "+
		`for example {{ with secret "kv/data/box/store" }}{{ .Data.data.token }}{{ end }}`)
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	storeURL := strings.TrimRight(strings.TrimSpace(*url), "/")
	if storeURL == "" {
		return usagef("box store set needs --url: the base URL of the store, for example https://images.example.com")
	}
	if !strings.HasPrefix(storeURL, "http://") && !strings.HasPrefix(storeURL, "https://") {
		return usagef("box store set --url %q is not an http(s) URL", storeURL)
	}
	if *fromFile != "" && *template != "" {
		return usagef("box store set: --from-file and --template are two ways to give the token, give one")
	}
	items := map[string]string{"url": storeURL}
	switch {
	case *fromFile != "":
		data, err := os.ReadFile(*fromFile)
		if err != nil {
			return wrapf(err, "cannot read %s", *fromFile)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return usagef("%s is empty: a store token is the content of the file", *fromFile)
		}
		items["token"] = token
	case *template != "":
		data, err := os.ReadFile(*template)
		if err != nil {
			return wrapf(err, "cannot read %s", *template)
		}
		text := string(data)
		if !strings.Contains(text, "{{") {
			return usagef("%s holds no template text: such an item is a Nomad template, for example "+
				`{{ with secret "kv/data/box/store" }}{{ .Data.data.token }}{{ end }}, and it must never hold the token `+
				"itself (name it with --from-file to store the token)", *template)
		}
		items["token.tmpl"] = text
	}
	size := 0
	for _, value := range items {
		size += len(value)
	}
	if size > boxExecutorsVariableLimit {
		return usagef("the store's Variable would be %d bytes, more than the %d Nomad accepts for one variable; "+
			"use --template so that it holds a path instead of the token", size, boxExecutorsVariableLimit)
	}
	written, err := a.Nomad.VariableWrite(ctx, &api.Variable{Path: boxStoreVariable, Items: items})
	if err != nil {
		return wrapf(err, "Nomad refused the variable %s%s", boxStoreVariable, accessDeniedHint(err))
	}
	out := storeOutput{URL: storeURL, Configured: true, Token: tokenKind(items)}
	out.Items = itemNames(items)
	if written != nil {
		out.URL = strings.TrimSpace(written.Items["url"])
	}
	if a.JSON {
		return a.printJSON(out)
	}
	a.note("the image store is %s (variable %s)", out.URL, boxStoreVariable)
	a.note("  token  %s", describeToken(out.Token))
	a.note("a publish reads it: `box images publish <version> --yes`")
	return nil
}

// cmdStoreShow prints the store's location and how its token reaches a publish,
// and never the token itself.
func (a *App) cmdStoreShow(ctx context.Context, args []string) error {
	fs := newFlagSet("store show")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	v, err := a.Nomad.VariableRead(ctx, boxStoreVariable)
	if err != nil {
		if isNotFoundErr(err) {
			v = nil
		} else {
			return wrapf(err, "cannot read the variable %s", boxStoreVariable)
		}
	}
	out := storeOutput{}
	if v != nil {
		out.URL = strings.TrimSpace(v.Items["url"])
		out.Token = tokenKind(v.Items)
		out.Items = itemNames(v.Items)
		out.Configured = out.URL != ""
	}
	if a.JSON {
		return a.printJSON(out)
	}
	if !out.Configured {
		a.note("no image store is configured: `box store set --url URL --from-file FILE` writes the variable %s", boxStoreVariable)
		return nil
	}
	a.note("the image store is %s (variable %s)", out.URL, boxStoreVariable)
	a.note("  token  %s", describeToken(out.Token))
	a.note("  items  %s", strings.Join(out.Items, ", "))
	return nil
}

// tokenKind says how a store's token is held, from the item names: never a value.
func tokenKind(items map[string]string) string {
	switch {
	case items["token.tmpl"] != "":
		return "template"
	case items["token"] != "":
		return "variable"
	}
	return ""
}

// describeToken renders how the token reaches a publish task.
func describeToken(kind string) string {
	switch kind {
	case "template":
		return "read from Vault when the task renders (the Variable holds a path, not the token)"
	case "variable":
		return "stored in the Variable (use --template to keep it in Vault instead)"
	}
	return "none: the store is written without authentication"
}

// itemNames lists a Variable's item names, sorted and without values.
func itemNames(items map[string]string) []string {
	out := make([]string, 0, len(items))
	for key := range items {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// storeItems turns the store Variable into the items a publish job renders.
func storeItems(v *api.Variable) []ExecutorItem {
	if v == nil {
		return nil
	}
	var out []ExecutorItem
	for _, key := range storeTemplateKeys {
		value, ok := v.Items[key]
		if !ok {
			continue
		}
		item := ExecutorItem{Key: key}
		if isTemplateKey(key) {
			item.Text = value
		}
		out = append(out, item)
	}
	return out
}

// accessDeniedHint says what to do when Nomad refuses the variable.
func accessDeniedHint(err error) string {
	if isPermissionDeniedErr(err) {
		return " (the token needs variables write on nomad/jobs/box-store: deploy/acl/box-operator.policy.hcl grants it)"
	}
	return ""
}
