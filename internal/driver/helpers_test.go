package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/stretchr/testify/require"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// attrValue reads a string fingerprint attribute.
func attrValue(t *testing.T, fp *drivers.Fingerprint, key string) string {
	t.Helper()
	attr, ok := fp.Attributes[key]
	require.True(t, ok, "missing fingerprint attribute %q", key)
	value, ok := attr.GetString()
	require.True(t, ok, "attribute %q is not a string", key)
	return value
}
