package driver

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"os"
	"path/filepath"
	"testing"
)

func TestExamplesAreValidHCL(t *testing.T) {
	files, e := filepath.Glob("../../examples/*.hcl")
	if e != nil || len(files) < 2 {
		t.Fatal(e)
	}
	for _, p := range files {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		_, diags := hclsyntax.ParseConfig(b, p, hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatal(diags.Error())
		}
	}
}
