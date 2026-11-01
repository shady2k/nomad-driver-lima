package lima

import (
	"strings"
	"testing"
)

func TestPTYCommandQuoting(t *testing.T) {
	v, e := quoteCommand([]string{"printf", "a'b;$(id)"})
	if e != nil || !strings.Contains(v, "'\"'\"'") {
		t.Fatal(v, e)
	}
	if _, e = quoteCommand([]string{"x\x00"}); e == nil {
		t.Fatal("NUL accepted")
	}
}
