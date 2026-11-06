package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// helpError is a command's -h: Run prints the command's flags and exits 0.
type helpError struct{ fs *flag.FlagSet }

func (e *helpError) Error() string { return e.fs.Name() + ": help requested" }

// parseError turns a parse failure into the command's usage error, or into its
// help when -h was asked for.
func parseError(fs *flag.FlagSet, err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return &helpError{fs: fs}
	}
	return usagef("%s: %v; `%s --help` lists its flags", fs.Name(), err, fs.Name())
}

// printFlags writes a flag set's flags, one per line, with their defaults.
func printFlags(w io.Writer, fs *flag.FlagSet) {
	var lines []string
	fs.VisitAll(func(f *flag.Flag) {
		name := "--" + f.Name
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
			name += " " + flagPlaceholder(f)
		}
		line := fmt.Sprintf("  %-22s %s", name, f.Usage)
		if f.DefValue != "" && f.DefValue != "0" && f.DefValue != "false" {
			line += fmt.Sprintf(" (default %s)", f.DefValue)
		}
		lines = append(lines, line)
	})
	if len(lines) == 0 {
		fmt.Fprintf(w, "%s takes no flags\n", fs.Name())
		return
	}
	fmt.Fprintf(w, "flags of %s:\n%s\n", fs.Name(), strings.Join(lines, "\n"))
}

// flagPlaceholder names the value a flag takes, from its type.
func flagPlaceholder(f *flag.Flag) string {
	switch fmt.Sprintf("%T", f.Value) {
	case "*flag.intValue", "*flag.int64Value", "*flag.uintValue":
		return "N"
	case "*flag.durationValue":
		return "DURATION"
	default:
		return "VALUE"
	}
}

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
		return parseError(fs, err)
	}
	rest := fs.Args()
	if want >= 0 && len(rest) != want {
		got := ""
		if len(rest) > 0 {
			got = " (" + strings.Join(rest, " ") + ")"
		}
		return usagef("%s: expected %d argument(s), got %d%s; `%s --help` says which, and lists the flags",
			fs.Name(), want, len(rest), got, fs.Name())
	}
	return nil
}

// parseFlagsMin parses args and checks that at least min positional arguments
// are present.
func parseFlagsMin(fs *flag.FlagSet, args []string, min int) error {
	args = splitInterspersed(fs, args)
	if err := fs.Parse(args); err != nil {
		return parseError(fs, err)
	}
	if len(fs.Args()) < min {
		return usagef("%s: expected at least %d argument(s), got %d; `%s --help` says which, and lists the flags",
			fs.Name(), min, len(fs.Args()), fs.Name())
	}
	return nil
}

// optionalDuration is a flag whose value may be left out: "--wait" is the flag's
// own meaning, "--wait 30m" also carries a duration.
//
// It is a boolean flag to the flag package, which is what lets it be written
// without a value at all. joinOptionalValue does the other half: it joins a
// duration that follows into "--wait=30m", because the flag package never takes
// the next argument for a boolean flag.
type optionalDuration struct {
	// set is false until the flag was given on the command line.
	set bool
	// on is the flag's own meaning: true for "--wait", false for "--wait=false".
	on bool
	// d is the duration it was given, 0 for none.
	d time.Duration
}

// String renders the value the way the flag package prints its defaults.
func (o *optionalDuration) String() string {
	if o.d > 0 {
		return o.d.String()
	}
	return strconv.FormatBool(o.on)
}

// IsBoolFlag is what makes "--wait" alone work.
func (o *optionalDuration) IsBoolFlag() bool { return true }

// Set reads "true", "false" or a duration.
func (o *optionalDuration) Set(value string) error {
	o.set = true
	switch strings.TrimSpace(value) {
	case "", "true":
		o.on = true
		return nil
	case "false":
		o.on = false
		return nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("want a duration such as 30m, or =false, got %q", value)
	}
	if d <= 0 {
		return fmt.Errorf("want a positive duration, got %q", value)
	}
	o.on, o.d = true, d
	return nil
}

// given reports whether the flag was on the command line.
func (o *optionalDuration) given() bool { return o.set }

// joinOptionalValue turns "--name VALUE" into "--name=VALUE" when VALUE is a
// value of the flag: a flag whose value is optional cannot rely on the flag
// package for that, and its value would otherwise be read as an argument.
func joinOptionalValue(args []string, name string, takes func(string) bool) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// Everything after -- belongs to the command that follows.
			return append(out, args[i:]...)
		}
		if (arg == "--"+name || arg == "-"+name) && i+1 < len(args) && takes(args[i+1]) {
			out = append(out, arg+"="+args[i+1])
			i++
			continue
		}
		out = append(out, arg)
	}
	return out
}

// isDurationValue reports whether an argument is a duration, which is what makes
// it the value of an optional-duration flag rather than an argument of the
// command.
func isDurationValue(value string) bool {
	_, err := time.ParseDuration(value)
	return err == nil
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
