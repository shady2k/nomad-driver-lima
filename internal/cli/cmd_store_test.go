package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// TestStoreSetAndShowCarryTheTokenPlace: which header the token goes in, and what
// comes before it, are part of the store. They are not secrets, an empty scheme
// is a store that wants a bare key, and the token is never printed.
func TestStoreSetAndShowCarryTheTokenPlace(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("s3cret-token\n"), 0o600))

	require.Equal(t, ExitOK, ta.run(t, "store", "set", "--url", "https://store.example.com/",
		"--from-file", tokenFile, "--token-header", "X-Api-Key", "--token-scheme", ""), ta.errOut.String())
	variable := ta.cluster.variables[boxStoreVariable]
	require.NotNil(t, variable)
	require.Equal(t, "X-Api-Key", variable.Items["header"])
	require.Equal(t, "", variable.Items["scheme"], "an empty scheme is written, not left out")
	require.Equal(t, "https://store.example.com", variable.Items["url"])
	require.NotContains(t, ta.out.String()+ta.errOut.String(), "s3cret-token")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "store", "show"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "X-Api-Key")
	require.Contains(t, out, "the token alone", "an empty scheme is a bare key header")
	require.NotContains(t, out, "s3cret-token")
}

// TestStoreShowSaysTheDefaults: a store written before the header and the scheme
// existed reads as the defaults of images/publish.sh.
func TestStoreShowSaysTheDefaults(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.variables[boxStoreVariable] = &api.Variable{Path: boxStoreVariable, Items: map[string]string{
		"url":   "https://store.example.com",
		"token": "s3cret-token",
	}}
	require.Equal(t, ExitOK, ta.run(t, "--json", "store", "show"), ta.errOut.String())
	require.True(t, ta.JSON)
	out := ta.out.String()
	require.Contains(t, out, `"header": "Authorization"`)
	require.Contains(t, out, `"scheme": "Bearer"`)
	require.NotContains(t, out, "s3cret-token")
}

// TestStoreRefusesAnEmptyHeaderName: a token with nowhere to go is a usage error,
// not a store that fails at the first publish.
func TestStoreRefusesAnEmptyHeaderName(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitUsage, ta.run(t, "store", "set", "--url", "https://store.example.com",
		"--token-header", "", "--from-file", filepath.Join(t.TempDir(), "token")))
	require.Contains(t, ta.errOut.String(), "not an HTTP header name")
	require.Nil(t, ta.cluster.variables[boxStoreVariable])
}
