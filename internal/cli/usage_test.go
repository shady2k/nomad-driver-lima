package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// usageFlagPattern finds the flags a usage line names: "--name VALUE", "-f".
var usageFlagPattern = regexp.MustCompile(`(?:^|[\s(\[|;])--?([A-Za-z][A-Za-z0-9-]*)`)

// usageFlags returns the flag names one usage line names, each once.
func usageFlags(usage string) []string {
	seen := map[string]bool{}
	var flags []string
	for _, match := range usageFlagPattern.FindAllStringSubmatch(usage, -1) {
		if name := match[1]; !seen[name] {
			seen[name] = true
			flags = append(flags, name)
		}
	}
	return flags
}

// usageSubcommands returns the subcommands a usage line names after
// "box <command>": the words of "box executors set ..." and "box session
// new|ls|close ...".
func usageSubcommands(name, usage string) []string {
	pattern := regexp.MustCompile(`\bbox\s+` + regexp.QuoteMeta(name) + `\s+(\S+)`)
	seen := map[string]bool{}
	var subs []string
	for _, match := range pattern.FindAllStringSubmatch(usage, -1) {
		token := strings.Trim(match[1], ".,;:()[]")
		for _, sub := range strings.Split(token, "|") {
			sub = strings.TrimSpace(sub)
			if sub == "" || strings.HasPrefix(sub, "-") || strings.HasPrefix(sub, "<") || seen[sub] {
				continue
			}
			if !regexp.MustCompile(`^[a-z][a-z0-9-]*$`).MatchString(sub) {
				continue
			}
			seen[sub] = true
			subs = append(subs, sub)
		}
	}
	return subs
}

// flagSetsOf returns the flag sets a command builds for itself: the one "-h"
// reaches, and one per subcommand named in its usage line.
func flagSetsOf(ctx context.Context, a *App, name string, cmd command) []*flag.FlagSet {
	var sets []*flag.FlagSet
	add := func(args []string) {
		err := cmd.run(ctx, args)
		var help *helpError
		if errors.As(err, &help) {
			sets = append(sets, help.fs)
		}
	}
	add([]string{"-h"})
	if cmd.subcommands {
		for _, sub := range usageSubcommands(name, cmd.usage) {
			add([]string{sub, "-h"})
		}
	}
	return sets
}

// TestUsageNamesOnlyDefinedFlags: every flag a command's usage line names is a
// flag that command defines, so the help cannot promise a flag that exits 2.
//
// The command table is the only list of commands, and a command's flag set is
// made inside its run function, so the set is read by asking the command for
// its help: parseFlags answers -h with the set. A command whose first word is a
// subcommand is asked with each subcommand its usage line names, and the flags
// of all of them count as the command's. box run and box cp parse their
// arguments by hand and have no flag set at all (the table marks them noFlags),
// so they are the two commands this cannot check.
func TestUsageNamesOnlyDefinedFlags(t *testing.T) {
	ctx := context.Background()
	a := &App{Out: io.Discard, ErrOut: io.Discard}
	for name, cmd := range a.commands() {
		if cmd.noFlags {
			continue
		}
		defined := map[string]bool{}
		for _, fs := range flagSetsOf(ctx, a, name, cmd) {
			fs.VisitAll(func(f *flag.Flag) { defined[f.Name] = true })
		}
		for _, flagName := range usageFlags(cmd.usage) {
			if !defined[flagName] {
				t.Errorf("box %s: the usage line names --%s, which the command does not define (%s)",
					name, flagName, strings.Join(sortedFlagNames(defined), ", "))
			}
		}
	}
}

// sortedFlagNames returns a set of flag names in order, for a test's message.
func sortedFlagNames(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, "--"+key)
	}
	sort.Strings(keys)
	return keys
}
