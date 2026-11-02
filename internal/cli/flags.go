package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// newFlagSet returns a flag set that reports nothing itself; the command turns
// a parse failure into a usage error with the command's own words.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("box "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// splitInterspersed moves the positional arguments after the flags, so that
// "box create dev1 --image 1.0.0" works as well as "box create --image 1.0.0 dev1".
// The Go flag package stops at the first positional argument otherwise.
func splitInterspersed(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		flag := fs.Lookup(name)
		if flag == nil {
			continue
		}
		if boolFlag, ok := flag.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positionals...)
}

// parseFlags parses args and checks the number of positional arguments.
//
// want is the exact number of positional arguments, or -1 for any number.
func parseFlags(fs *flag.FlagSet, args []string, want int) error {
	args = splitInterspersed(fs, args)
	if err := fs.Parse(args); err != nil {
		return usagef("%s: %v (see `box help`)", fs.Name(), err)
	}
	rest := fs.Args()
	if want >= 0 && len(rest) != want {
		return usagef("%s: expected %d argument(s), got %d: %s", fs.Name(), want, len(rest), strings.Join(rest, " "))
	}
	return nil
}

// parseFlagsMin parses args and checks that at least min positional arguments
// are present.
func parseFlagsMin(fs *flag.FlagSet, args []string, min int) error {
	args = splitInterspersed(fs, args)
	if err := fs.Parse(args); err != nil {
		return usagef("%s: %v (see `box help`)", fs.Name(), err)
	}
	if len(fs.Args()) < min {
		return usagef("%s: expected at least %d argument(s), got %d", fs.Name(), min, len(fs.Args()))
	}
	return nil
}

// requirePositive turns a non-positive flag into a usage error.
func requirePositive(name string, v int) error {
	if v <= 0 {
		return usagef("--%s must be at least 1, got %d", name, v)
	}
	return nil
}

// describeCount renders "1 box" / "3 boxes".
func describeCount(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
