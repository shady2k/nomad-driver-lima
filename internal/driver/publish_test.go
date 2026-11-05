package driver

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// A publish talks to a store that may be behind a proxy, wants the token in a
// header of its own choosing and is shared with other publishers. Each of those
// is one test against the fake store in imagework_test.go.

// storeTaskConfigFiles is a publish task whose secrets hold the given store
// files. An empty value is an empty file, which the driver must not read as a
// missing one.
func storeTaskConfigFiles(t *testing.T, version, arch, sha, url string, files map[string]string) *drivers.TaskConfig {
	t.Helper()
	tc := taskConfig(t, t.TempDir(), func(tc *drivers.TaskConfig) { tc.Name = "publish" })
	dir := filepath.Join(tc.TaskDir().SecretsDir, storeDirInSecrets)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "url"), []byte(url+"\n"), 0o600))
	for name, value := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600))
	}
	require.NoError(t, tc.EncodeConcreteDriverConfig(TaskConfig{
		BoxID:   "project",
		Publish: &PublishConfig{Version: version, Arch: arch, SHA256: sha},
	}))
	return tc
}

// publishOne runs one publish of a baked image against a store and returns the
// exit result.
func publishOne(t *testing.T, store *fakeStore, files map[string]string) (*drivers.ExitResult, box.BakedRecord, *fakeStore) {
	t.Helper()
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	rec := putBakedImage(t, cache, box.BakedRecord{
		Version: "project-1", Arch: "amd64", Name: "project", Parent: "1.0.0",
		BuiltAt: "2026-11-02T12:00:00Z", Platform: "mount",
	}, []byte("QFI\xfb the image\n"))
	tc := storeTaskConfigFiles(t, "project-1", "amd64", rec.SHA256, srv.URL, files)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	return waitExit(t, d, "task1", 30*time.Second), rec, store
}

// TestPublishRetriesAChunkedUpload: a store (or the proxy in front of it) that
// refuses a body of a declared size gets the same bytes in chunked form, which is
// what images/publish.sh does. Without the retry the publish stops at the 413.
func TestPublishRetriesAChunkedUpload(t *testing.T) {
	store := newFakeStore()
	store.refuseDeclaredSize = true
	res, rec, store := publishOne(t, store, nil)
	require.True(t, res.Successful(), "the publish exited %+v", res)

	require.Equal(t, 1, store.declaredRefusals, "the declared-size upload was refused once")
	require.Equal(t, 1, store.chunkedUploads, "the image went up again without a Content-Length")
	require.Equal(t, []byte("QFI\xfb the image\n"),
		store.files["/box-base/project-1/amd64/box-base-project-1-amd64.qcow2"])

	var index map[string]any
	require.NoError(t, json.Unmarshal(store.files["/box-base/index.json"], &index))
	entry := index["images"].(map[string]any)["project-1"].(map[string]any)["amd64"].(map[string]any)
	require.Equal(t, rec.SHA256, entry["sha256"])
}

// TestPublishRetriesWhenTheStoreClosesOnTheRefusal: a proxy answers 413 and
// closes the connection while the image is still being written, so the upload
// ends in a write error and no status at all. The publish must still take the
// chunked form -- measured against a real store, this is the case that stopped
// every publish of an 847 MiB image.
func TestPublishRetriesWhenTheStoreClosesOnTheRefusal(t *testing.T) {
	store := newFakeStore()
	store.refuseDeclaredSize = true
	store.closeOnDeclaredRefusal = true

	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	// Large enough to fill a socket buffer, so the closed connection is seen
	// while the body is still going out: a few bytes fit and the status arrives.
	image := make([]byte, 4<<20)
	copy(image, "QFI\xfb the image\n")
	rec := putBakedImage(t, cache, box.BakedRecord{
		Version: "project-1", Arch: "amd64", Name: "project", Parent: "1.0.0",
		BuiltAt: "2026-11-02T12:00:00Z", Platform: "mount",
	}, image)
	tc := storeTaskConfigFiles(t, "project-1", "amd64", rec.SHA256, srv.URL, nil)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
	require.True(t, res.Successful(), "the publish exited %+v", res)
	require.Equal(t, 1, store.declaredRefusals)
	require.Equal(t, 1, store.chunkedUploads, "the image went up again without a Content-Length")
	require.Len(t, store.files["/box-base/project-1/amd64/box-base-project-1-amd64.qcow2"], len(image))
}

// TestPublishSendsTheTokenTheStoreAsksFor: the header and the scheme of the
// credential are the store's, not hard-coded, and an empty scheme is a bare key.
func TestPublishSendsTheTokenTheStoreAsksFor(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		header string
		value  string
	}{
		{
			name:   "a header of its own with a scheme",
			files:  map[string]string{"token": "s3cret-token", "header": "X-Api-Key", "scheme": "Token"},
			header: "X-Api-Key",
			value:  "Token s3cret-token",
		},
		{
			name:   "a bare key",
			files:  map[string]string{"token": "s3cret-token", "header": "X-Api-Key", "scheme": ""},
			header: "X-Api-Key",
			value:  "s3cret-token",
		},
		{
			name:   "the defaults",
			files:  map[string]string{"token": "s3cret-token"},
			header: "Authorization",
			value:  "Bearer s3cret-token",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeStore()
			store.requiredHeader, store.requiredValue = c.header, c.value
			res, _, store := publishOne(t, store, c.files)
			require.True(t, res.Successful(), "the publish exited %+v", res)
			require.NotEmpty(t, store.headers)
			for _, h := range store.headers {
				if c.header != "Authorization" {
					require.Empty(t, h.Get("Authorization"), "the credential went to the default header as well")
				}
				require.Contains(t, h.Get(c.header), c.value)
			}
		})
	}
}

// TestPublishRefusesAnEmptyHeaderName: a token with nowhere to go is said before
// the first request, not left to an HTTP client error.
func TestPublishRefusesAnEmptyHeaderName(t *testing.T) {
	store := newFakeStore()
	res, _, store := publishOne(t, store, map[string]string{"token": "s3cret-token", "header": " "})
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "token header")
	require.Contains(t, res.Err.Error(), "empty")
	require.Empty(t, store.files, "nothing was written to the store")
}

// TestPublishRefusesTheLockOfAnotherPublisher: the lock is box-base/.lock, the
// same object images/publish.sh takes, and a fresh one stops the publish before
// anything is uploaded.
func TestPublishRefusesTheLockOfAnotherPublisher(t *testing.T) {
	store := newFakeStore()
	store.files["/box-base/.lock"] = []byte("state=held\nowner=elsewhere-42\npid=42\nversion=x\narch=amd64\ntime=" +
		strconv.FormatInt(time.Now().Unix(), 10) + "\n")
	res, _, store := publishOne(t, store, nil)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "held by elsewhere-42")
	require.Contains(t, res.Err.Error(), "another publisher is running")
	require.NotContains(t, store.files, "/box-base/project-1/amd64/box-base-project-1-amd64.qcow2",
		"nothing is uploaded under someone else's lock")
}

// TestPublishTakesOverAStaleLockAndReleasesIt: a run that died must not block the
// store for ever, and a finished publish leaves a record that blocks nobody.
func TestPublishTakesOverAStaleLockAndReleasesIt(t *testing.T) {
	store := newFakeStore()
	store.files["/box-base/.lock"] = []byte("state=held\nowner=elsewhere-42\npid=42\nversion=x\narch=amd64\ntime=" +
		strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) + "\n")
	res, _, store := publishOne(t, store, nil)
	require.True(t, res.Successful(), "the publish exited %+v", res)
	require.Equal(t, "GET /box-base/.lock", store.requests[0],
		"the lock is looked at before anything else is written")
	lock := string(store.files["/box-base/.lock"])
	require.Contains(t, lock, "state=released", "the lock is released when the publish ends")
	require.Contains(t, lock, "version=project-1")
}

// TestPublishRefusesAnImageThatChangedInTheCache: the store cannot take an upload
// back, so the file is checked against its record before it is sent.
func TestPublishRefusesAnImageThatChangedInTheCache(t *testing.T) {
	store := newFakeStore()
	srv := httptest.NewServer(store)
	defer srv.Close()
	d, cache := driverWithQEMUImg(t, fakeQEMUImg(t, okQEMUImg))
	rec := putBakedImage(t, cache, box.BakedRecord{Version: "project-1", Arch: "amd64", Name: "project", Platform: "mount"},
		[]byte("QFI\xfb the image\n"))
	require.NoError(t, os.WriteFile(filepath.Join(cache, rec.SHA256+".img"), []byte("QFI\xfb something else\n"), 0o644))
	tc := storeTaskConfigFiles(t, "project-1", "amd64", rec.SHA256, srv.URL, nil)
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)
	res := waitExit(t, d, "task1", 30*time.Second)
	require.False(t, res.Successful())
	require.Contains(t, res.Err.Error(), "not what was baked")
	require.NotContains(t, store.files, "/box-base/project-1/amd64/box-base-project-1-amd64.qcow2",
		"a file that changed under its record is never uploaded")
	require.NotContains(t, store.files, "/box-base/index.json")
}
