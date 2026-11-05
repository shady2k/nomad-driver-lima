package driver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// Publishing an image distributes it, so it is its own task: the image is on the
// node that baked it, and the store is reached from that node. The task block
// names the version, the architecture and the digest, and the task's secrets carry
// the store: its url, and the token when it needs one. A token is never an
// argument, never in an event and never in a log.
//
// The order is the one images/publish.sh uses, and for the same reasons:
//
//	1. refuse a version that is already published (a version is immutable);
//	2. upload the image, its .sha256 and its manifest;
//	3. read the image back and compare its sha256, and the two small files byte
//	   for byte: the index never names an image that did not arrive;
//	4. re-read index.json, merge this version in, write it, read it back and
//	   verify that this entry and every entry that was there before survived.

// storeDirInSecrets is where the task's secrets carry the store: url, and token
// when the store needs one.
const storeDirInSecrets = "store"

// storeRequestTimeout bounds one request that is not an upload.
const storeRequestTimeout = 60 * time.Second

// storeConfig is the operator's store as the task's secrets give it.
type storeConfig struct {
	// URL is the base URL of the store; the box-base/ layout is written below it.
	URL string
	// Token is the write token, empty for a store that needs none.
	Token string
	// Header is the request header the token goes in: BOX_STORE_TOKEN_HEADER of
	// images/publish.sh, and Authorization when the store says nothing.
	Header string
	// Scheme is the word in front of the token: BOX_STORE_TOKEN_SCHEME of
	// images/publish.sh, and Bearer when the store says nothing. It is empty for
	// a store that wants a bare key header, which is why a scheme file that is
	// there and empty is not the same as a file that is not there.
	Scheme string
}

// The store's defaults, the ones images/publish.sh uses.
const (
	storeHeaderDefault = "Authorization"
	storeSchemeDefault = "Bearer"
)

// readStore reads the store out of the task's secrets directory. The token is
// read once, and nothing here prints it.
func readStore(secretsDir string) (storeConfig, error) {
	dir := filepath.Join(secretsDir, storeDirInSecrets)
	url, found, err := readStoreFile(dir, "url")
	if err != nil {
		return storeConfig{}, fmt.Errorf("read the store url: %w", err)
	}
	if !found {
		return storeConfig{}, fmt.Errorf("this node has no image store configured: the task's secrets carry no %s/url. "+
			"`box store set --url URL --from-file FILE` writes it", storeDirInSecrets)
	}
	store := storeConfig{
		URL:    strings.TrimRight(strings.TrimSpace(url), "/"),
		Header: storeHeaderDefault,
		Scheme: storeSchemeDefault,
	}
	if store.URL == "" {
		return storeConfig{}, fmt.Errorf("the image store has no url: `box store set --url URL --from-file FILE` writes it")
	}
	if token, ok, err := readStoreFile(dir, "token"); err != nil {
		return storeConfig{}, fmt.Errorf("read the store token: %w", err)
	} else if ok {
		store.Token = token
	}
	if header, ok, err := readStoreFile(dir, "header"); err != nil {
		return storeConfig{}, fmt.Errorf("read the store header: %w", err)
	} else if ok {
		store.Header = strings.TrimSpace(header)
	}
	if scheme, ok, err := readStoreFile(dir, "scheme"); err != nil {
		return storeConfig{}, fmt.Errorf("read the store scheme: %w", err)
	} else if ok {
		store.Scheme = strings.TrimSpace(scheme)
	}
	if store.Token != "" {
		// A credential with nowhere to go is worth a sentence before the upload.
		if problem := headerNameProblem(store.Header); problem != "" {
			return storeConfig{}, fmt.Errorf("the store's token header: %s. `box store set --token-header NAME` writes it", problem)
		}
		if strings.ContainsAny(store.Scheme, " \t\r\n") {
			return storeConfig{}, fmt.Errorf("the store's token scheme %q holds whitespace: it is the word in front of the token, "+
				"and empty for a bare key. `box store set --token-scheme WORD` writes it", store.Scheme)
		}
	}
	return store, nil
}

// readStoreFile reads one file of the store directory and trims the newline a
// template leaves behind. A file that is not there is reported as absent, not as
// empty: an empty scheme asks the store for a bare key.
func readStoreFile(dir, name string) (string, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimRight(string(b), "\n"), true, nil
}

// headerNameProblem says why a name is not an HTTP header name, or "" when it is.
func headerNameProblem(name string) string {
	if name == "" {
		return "the name is empty"
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return fmt.Sprintf("%q is not a header name", name)
		}
	}
	return ""
}

// publisher writes one image into a store.
type publisher struct {
	store  storeConfig
	client *http.Client
	emit   func(kind, message string)
}

// newPublisher builds a publisher; emit receives the progress lines.
func newPublisher(store storeConfig, emit func(kind, message string)) *publisher {
	return &publisher{store: store, client: &http.Client{Timeout: 0}, emit: emit}
}

// imagePaths names the three files of one version in the store, and the index.
func imagePaths(baseURL, version, arch string) (versionPath, artifact, sha, manifest, index string) {
	versionPath = fmt.Sprintf("%s/box-base/%s/%s", baseURL, version, arch)
	artifact = fmt.Sprintf("box-base-%s-%s.qcow2", version, arch)
	return versionPath, artifact, artifact + ".sha256", fmt.Sprintf("box-base-%s-%s.json", version, arch),
		baseURL + "/box-base/index.json"
}

// The store's lock is one more object in it, box-base/.lock, and it is the lock
// images/publish.sh takes: every publisher of the store sees it, so two runs do
// not lose each other's index entry. A record says "held" or "released" and who
// wrote it when; the holder refreshes it after every upload, and a held record
// older than the staleness below is taken over, so a crashed run cannot block the
// store for ever.
const (
	lockObject = "box-base/.lock"
	lockStale  = 30 * time.Minute
)

// storeLock is this publisher's hold on one store.
type storeLock struct {
	// url is the lock object itself.
	url string
	// owner names this run, the way images/publish.sh names itself: host and pid.
	owner string
	// version and arch are in the record, so a stale one says who left it.
	version, arch string
}

// acquireLock takes the store's lock for one publish.
//
// The document is written, read back and compared, so two publishers that wrote
// at the same moment notice that one of them lost: the same steps, and the same
// record, as images/publish.sh.
func (p *publisher) acquireLock(ctx context.Context, version, arch string) (*storeLock, error) {
	lock := &storeLock{url: p.store.URL + "/" + lockObject, owner: lockOwner(), version: version, arch: arch}
	current, found, err := p.lockState(ctx, lock.url)
	if err != nil {
		return nil, fmt.Errorf("cannot read the lock %s: %w", lock.url, err)
	}
	if found {
		since, parseErr := strconv.ParseInt(lockField(current, "time"), 10, 64)
		if held := time.Since(time.Unix(since, 0)); lockField(current, "state") == "held" && parseErr == nil {
			if held < lockStale {
				return nil, fmt.Errorf("%s is held by %s since %s (%d min ago): another publisher is running. "+
					"Wait for it, or delete the lock once you know that run is gone",
					lock.url, lockField(current, "owner"),
					time.Unix(since, 0).UTC().Format("2006-01-02T15:04:05Z"), int(held.Minutes()))
			}
			p.emit("publish", fmt.Sprintf("Taking over a stale lock: held by %s for %d min (stale after %d min)",
				lockField(current, "owner"), int(held.Minutes()), int(lockStale.Minutes())))
		}
	}
	if err := p.putLock(ctx, lock, "held"); err != nil {
		return nil, err
	}
	p.emit("publish", fmt.Sprintf("Locked %s as %s", lockObject, lock.owner))
	return lock, nil
}

// refreshLock rewrites the lock as held: an upload of a gigabyte takes long
// enough that a run must not look abandoned while it works.
func (p *publisher) refreshLock(ctx context.Context, lock *storeLock) error {
	return p.putLock(ctx, lock, "held")
}

// releaseLock leaves a record that blocks nobody. A store that cannot take it is
// not worth failing a published image over.
func (p *publisher) releaseLock(ctx context.Context, lock *storeLock) {
	if err := p.putLock(context.WithoutCancel(ctx), lock, "released"); err != nil {
		p.emit("publish", fmt.Sprintf("The lock %s was not released: %v", lockObject, err))
	}
}

// putLock writes one lock document and reads it back.
func (p *publisher) putLock(ctx context.Context, lock *storeLock, state string) error {
	doc := lockDocument(state, lock)
	ctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, lock.url, strings.NewReader(doc))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(doc))
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot write the lock %s: %w", lock.url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("cannot write the lock %s: the store answered HTTP %d", lock.url, resp.StatusCode)
	}
	got, found, err := p.lockState(ctx, lock.url)
	if err != nil {
		return fmt.Errorf("cannot read the lock %s back, so this publish would not know that it holds it: %w", lock.url, err)
	}
	if !found || got != doc {
		return fmt.Errorf("another publisher took the lock %s while this one wrote it:\n%s"+
			"Wait for that run to finish, or for the lock to go stale after %d minutes", lock.url, got, int(lockStale.Minutes()))
	}
	return nil
}

// lockState reads the lock document. A store that has none answers 404, which is
// a store nobody is publishing to.
func (p *publisher) lockState(ctx context.Context, url string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return string(b), true, err
	case http.StatusNotFound:
		return "", false, nil
	}
	return "", false, fmt.Errorf("the store answered HTTP %d", resp.StatusCode)
}

// lockOwner names this run in the lock record: the host and the process.
func lockOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// lockDocument is the record under box-base/.lock, in the format
// images/publish.sh writes: the two publish the same store, so they take the same
// lock, and each one reads the other's record.
func lockDocument(state string, lock *storeLock) string {
	return fmt.Sprintf("state=%s\nowner=%s\npid=%d\nversion=%s\narch=%s\ntime=%d\n",
		state, lock.owner, os.Getpid(), lock.version, lock.arch, time.Now().UTC().Unix())
}

// lockField reads one field out of a lock document.
func lockField(doc, field string) string {
	for _, line := range strings.Split(doc, "\n") {
		if value, ok := strings.CutPrefix(line, field+"="); ok {
			return value
		}
	}
	return ""
}

// publishImage uploads one baked image and names it in the index.
func (p *publisher) publishImage(ctx context.Context, rec box.BakedRecord, imagePath string) error {
	versionPath, artifactName, shaName, manifestName, indexURL := imagePaths(p.store.URL, rec.Version, rec.Arch)
	artifactURL := versionPath + "/" + artifactName

	// 0. Take the store's lock: two publishes of one store must not lose each
	// other's index entry, and the shell publisher takes the same one.
	lock, err := p.acquireLock(ctx, rec.Version, rec.Arch)
	if err != nil {
		return err
	}
	defer p.releaseLock(ctx, lock)

	// 1. A version is immutable: every file of it must be absent first.
	p.emit("publish", fmt.Sprintf("Checking that the image %s %s is not published yet in %s", rec.Version, rec.Arch, p.store.URL))
	for _, name := range []string{artifactName, shaName, manifestName} {
		status, err := p.status(ctx, http.MethodHead, versionPath+"/"+name)
		if err != nil {
			return fmt.Errorf("cannot check %s/%s: %w", versionPath, name, err)
		}
		switch status {
		case http.StatusOK:
			return fmt.Errorf("%s/%s already exists: %s %s is published in this store, and a published version never changes. "+
				"Bake another version instead", versionPath, name, rec.Version, rec.Arch)
		case http.StatusNotFound:
		default:
			return fmt.Errorf("cannot check %s/%s: the store answered HTTP %d", versionPath, name, status)
		}
	}
	if listed, err := p.indexLists(ctx, indexURL, rec.Version, rec.Arch); err != nil {
		return err
	} else if listed {
		return fmt.Errorf("index.json already lists %s %s: the version is published, so bake another one", rec.Version, rec.Arch)
	}

	// 2. Upload the image, its digest and its manifest, after checking that the
	// file in the cache still is the image its record names: a store that cannot
	// take an upload back must not be given a file that changed under the record.
	p.emit("publish", fmt.Sprintf("Checking that %s still is the image of sha256 %s", artifactName, rec.SHA256))
	local, localSize, err := fileDigest(imagePath)
	if err != nil {
		return fmt.Errorf("reading the image %s: %w", artifactName, err)
	}
	if local != rec.SHA256 {
		return fmt.Errorf("the image %s in this node's cache has sha256 %s, not the %s of its record: "+
			"it is not what was baked, so nothing was uploaded. Bake the box again", artifactName, local, rec.SHA256)
	}
	if rec.Size > 0 && localSize != rec.Size {
		return fmt.Errorf("the image %s in this node's cache is %d bytes, not the %d of its record: "+
			"nothing was uploaded. Bake the box again", artifactName, localSize, rec.Size)
	}
	shaFile := fmt.Sprintf("%s  %s\n", rec.SHA256, artifactName)
	manifest, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	manifest = append(manifest, '\n')
	p.emit("publish", fmt.Sprintf("Uploading the image %s (%s) to %s", artifactName, humanSize(rec.Size), artifactURL))
	if err := p.putFile(ctx, artifactURL, imagePath, rec.Size); err != nil {
		return fmt.Errorf("uploading %s: %w", artifactName, err)
	}
	if err := p.refreshLock(ctx, lock); err != nil {
		return err
	}
	if err := p.putBytes(ctx, versionPath+"/"+shaName, []byte(shaFile)); err != nil {
		return fmt.Errorf("uploading %s: %w", shaName, err)
	}
	if err := p.refreshLock(ctx, lock); err != nil {
		return err
	}
	if err := p.putBytes(ctx, versionPath+"/"+manifestName, manifest); err != nil {
		return fmt.Errorf("uploading %s: %w", manifestName, err)
	}
	if err := p.refreshLock(ctx, lock); err != nil {
		return err
	}

	// 3. Read it back before the index names it.
	p.emit("publish", "Reading the image back and comparing its sha256")
	got, size, err := p.digestOf(ctx, artifactURL)
	if err != nil {
		return fmt.Errorf("reading %s back: %w", artifactName, err)
	}
	if got != rec.SHA256 {
		return fmt.Errorf("the published image has sha256 %s, want %s: index.json was not touched", got, rec.SHA256)
	}
	if rec.Size > 0 && size != rec.Size {
		return fmt.Errorf("the published image is %d bytes, want %d: index.json was not touched", size, rec.Size)
	}
	for name, want := range map[string][]byte{shaName: []byte(shaFile), manifestName: manifest} {
		got, err := p.getBytes(ctx, versionPath+"/"+name)
		if err != nil {
			return fmt.Errorf("reading %s back: %w", name, err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("the published %s differs from what was uploaded: index.json was not touched", name)
		}
	}

	// 4. Merge the version into the index, write it, read it back.
	p.emit("publish", fmt.Sprintf("Naming the image in %s", indexURL))
	before, err := p.readIndex(ctx, indexURL)
	if err != nil {
		return err
	}
	merged, err := mergeIndex(before, rec, versionPath+"/"+artifactName, versionPath+"/"+shaName, versionPath+"/"+manifestName, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := p.putBytes(ctx, indexURL, merged); err != nil {
		return fmt.Errorf("writing %s: %w", indexURL, err)
	}
	back, err := p.getBytes(ctx, indexURL)
	if err != nil {
		return fmt.Errorf("reading %s back: %w", indexURL, err)
	}
	if err := verifyIndex(back, before, rec); err != nil {
		return fmt.Errorf("%s: %w", indexURL, err)
	}
	p.emit("publish", fmt.Sprintf("index.json lists the image %s %s, and every entry that was there before is intact", rec.Version, rec.Arch))
	return nil
}

// status is the status code of one request.
func (p *publisher) status(ctx context.Context, method, url string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// authorize sets the store's credential in the header the store reads: what goes
// there is "<scheme> <token>", or the token alone for a store that wants a bare
// key. The token is put there and never written anywhere else.
func (p *publisher) authorize(req *http.Request) {
	if p.store.Token == "" {
		return
	}
	value := p.store.Token
	if p.store.Scheme != "" {
		value = p.store.Scheme + " " + p.store.Token
	}
	req.Header.Set(p.store.Header, value)
}

// putFile uploads one file. A store (or a proxy in front of it) may refuse a body
// of a declared size with HTTP 413, after reading about a megabyte of it; the
// same bytes go through in chunked form, which carries no size to refuse. It is
// what images/publish.sh does for the same reason, and the read-back below is
// what proves the store kept exactly the bytes it was sent.
func (p *publisher) putFile(ctx context.Context, url, path string, size int64) error {
	status, body, err := p.put(ctx, url, path, size)
	if err != nil {
		return err
	}
	if status == http.StatusRequestEntityTooLarge && size > 0 {
		p.emit("publish", fmt.Sprintf("The store refused %d bytes (HTTP 413); uploading in chunked form", size))
		status, body, err = p.put(ctx, url, path, -1)
		if err != nil {
			return err
		}
	}
	return putResult(status, body)
}

// put sends the file at path as the body of a PUT; a negative size asks for a
// chunked upload.
//
// The status and the first line of the answer come back whatever they are: 413 is
// not a failure of the upload but the store's way of asking for the chunked form.
func (p *publisher) put(ctx context.Context, url, path string, size int64) (int, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return 0, "", err
	}
	req.ContentLength = size
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, oneLineOf(string(answer)), nil
}

// putResult turns the status of an upload into an error, or nil.
func putResult(status int, body string) error {
	if status/100 == 2 {
		return nil
	}
	if body != "" {
		return fmt.Errorf("the store answered HTTP %d: %s", status, body)
	}
	return fmt.Errorf("the store answered HTTP %d", status)
}

// putBytes uploads a small document.
func (p *publisher) putBytes(ctx context.Context, url string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the store answered HTTP %d: %s", resp.StatusCode, oneLineOf(string(body)))
	}
	return nil
}

// getBytes downloads a small document.
func (p *publisher) getBytes(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// digestOf downloads a file and returns its sha256 and its size. The bytes are not
// kept: an image is up to a gigabyte.
func (p *publisher) digestOf(ctx context.Context, url string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// readIndex reads index.json; a store that has none yet answers 404, which is an
// empty index and not a failure.
func (p *publisher) readIndex(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", url, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	case http.StatusNotFound:
		return nil, nil
	}
	return nil, fmt.Errorf("cannot read %s: the store answered HTTP %d", url, resp.StatusCode)
}

// indexLists reports whether the index already names a version and architecture.
func (p *publisher) indexLists(ctx context.Context, url, version, arch string) (bool, error) {
	before, err := p.readIndex(ctx, url)
	if err != nil {
		return false, err
	}
	doc := map[string]any{}
	if len(before) > 0 {
		if err := json.Unmarshal(before, &doc); err != nil {
			return false, nil // an unreadable index is what mergeIndex reports
		}
	}
	images, _ := doc["images"].(map[string]any)
	arches, _ := images[version].(map[string]any)
	return arches[arch] != nil, nil
}

// mergeIndex adds one image to an index document and returns the document to
// write. Every entry that is already there is kept as it is.
func mergeIndex(before []byte, rec box.BakedRecord, artifactURL, shaURL, manifestURL string, now time.Time) ([]byte, error) {
	doc := map[string]any{}
	if len(before) > 0 {
		if err := json.Unmarshal(before, &doc); err != nil {
			return nil, fmt.Errorf("the store's index.json is not readable: %v", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	if _, ok := doc["schema"]; !ok {
		doc["schema"] = 1
	}
	images, _ := doc["images"].(map[string]any)
	if images == nil {
		images = map[string]any{}
		doc["images"] = images
	}
	arches, _ := images[rec.Version].(map[string]any)
	if arches == nil {
		arches = map[string]any{}
		images[rec.Version] = arches
	}
	entry := map[string]any{
		"url":      artifactURL,
		"sha256":   rec.SHA256,
		"size":     rec.Size,
		"kind":     "project",
		"name":     rec.Name,
		"platform": rec.Platform,
	}
	if rec.Size > 0 {
		entry["size"] = rec.Size
	}
	if rec.VirtualBytes > 0 {
		entry["virtual_bytes"] = rec.VirtualBytes
	}
	if rec.BuiltAt != "" {
		entry["built_at"] = rec.BuiltAt
	}
	if rec.Parent != "" {
		entry["parent"] = rec.Parent
	}
	if rec.Recipe != "" {
		entry["recipe"] = rec.Recipe
	}
	entry["sha256_url"] = shaURL
	entry["manifest_url"] = manifestURL
	if _, exists := arches[rec.Arch]; exists {
		return nil, fmt.Errorf("index.json already lists %s %s: the version is published", rec.Version, rec.Arch)
	}
	arches[rec.Arch] = entry
	doc["updated_at"] = now.UTC().Format("2006-01-02T15:04:05Z")
	return json.MarshalIndent(doc, "", "  ")
}

// verifyIndex checks the index that came back: this image is named, and every
// entry that was there before survived it.
func verifyIndex(back, before []byte, rec box.BakedRecord) error {
	got := map[string]any{}
	if err := json.Unmarshal(back, &got); err != nil {
		return fmt.Errorf("the index read back is not JSON: %v", err)
	}
	images, _ := got["images"].(map[string]any)
	arches, _ := images[rec.Version].(map[string]any)
	entry, _ := arches[rec.Arch].(map[string]any)
	if entry == nil {
		return fmt.Errorf("the index read back does not list %s %s", rec.Version, rec.Arch)
	}
	if entry["sha256"] != rec.SHA256 {
		return fmt.Errorf("the index lists sha256 %v, wrote %s", entry["sha256"], rec.SHA256)
	}
	if len(before) == 0 {
		return nil
	}
	old := map[string]any{}
	if err := json.Unmarshal(before, &old); err != nil {
		return nil
	}
	for section, versions := range old {
		if section == "updated_at" || section == "schema" {
			continue
		}
		want, _ := versions.(map[string]any)
		have, _ := got[section].(map[string]any)
		for version, archEntries := range want {
			wantArches, _ := archEntries.(map[string]any)
			for arch, oldEntry := range wantArches {
				if section == "images" && version == rec.Version && arch == rec.Arch {
					continue
				}
				haveArches, _ := have[version].(map[string]any)
				now, _ := haveArches[arch].(map[string]any)
				if now == nil {
					return fmt.Errorf("the index lost the %s entry %s %s while this publish ran", section, version, arch)
				}
				// The entry must be the one that was there, not another image
				// under the same name: a concurrent writer that replaced it is
				// what this comparison catches.
				was, _ := oldEntry.(map[string]any)
				if was["sha256"] != now["sha256"] {
					return fmt.Errorf("the index changed the %s entry %s %s while this publish ran (sha256 %v became %v)",
						section, version, arch, was["sha256"], now["sha256"])
				}
			}
		}
	}
	return nil
}

// fileDigest is the sha256 and the size of a local file: what the store's copy
// is compared with, and what the record says the image is.
func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// oneLineOf is the first line of a response body, for an error message.
func oneLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// humanSize renders a byte count the way a task event reads it.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
