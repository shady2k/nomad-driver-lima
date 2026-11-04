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
}

// readStore reads the store out of the task's secrets directory. The token is
// read once, and nothing here prints it.
func readStore(secretsDir string) (storeConfig, error) {
	dir := filepath.Join(secretsDir, storeDirInSecrets)
	b, err := os.ReadFile(filepath.Join(dir, "url"))
	if err != nil {
		if os.IsNotExist(err) {
			return storeConfig{}, fmt.Errorf("this node has no image store configured: the task's secrets carry no %s/url. "+
				"`box store set --url URL --from-file FILE` writes it", storeDirInSecrets)
		}
		return storeConfig{}, fmt.Errorf("read the store url: %w", err)
	}
	store := storeConfig{URL: strings.TrimRight(strings.TrimSpace(string(b)), "/")}
	if store.URL == "" {
		return storeConfig{}, fmt.Errorf("the image store has no url: `box store set --url URL --from-file FILE` writes it")
	}
	if token, err := os.ReadFile(filepath.Join(dir, "token")); err == nil {
		store.Token = strings.TrimRight(string(token), "\n")
	} else if !os.IsNotExist(err) {
		return storeConfig{}, fmt.Errorf("read the store token: %w", err)
	}
	return store, nil
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

// publishImage uploads one baked image and names it in the index.
func (p *publisher) publishImage(ctx context.Context, rec box.BakedRecord, imagePath string) error {
	versionPath, artifactName, shaName, manifestName, indexURL := imagePaths(p.store.URL, rec.Version, rec.Arch)
	artifactURL := versionPath + "/" + artifactName

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

	// 2. Upload the image, its digest and its manifest.
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
	if err := p.putBytes(ctx, versionPath+"/"+shaName, []byte(shaFile)); err != nil {
		return fmt.Errorf("uploading %s: %w", shaName, err)
	}
	if err := p.putBytes(ctx, versionPath+"/"+manifestName, manifest); err != nil {
		return fmt.Errorf("uploading %s: %w", manifestName, err)
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

// authorize sets the store's credential. The token is put in the header and never
// written anywhere else.
func (p *publisher) authorize(req *http.Request) {
	if p.store.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.store.Token)
	}
}

// putFile uploads one file. A store (or a proxy in front of it) may refuse a body
// of a declared size with HTTP 413; the same bytes go through in chunked form,
// which is what images/publish.sh does for the same reason.
func (p *publisher) putFile(ctx context.Context, url, path string, size int64) error {
	status, err := p.put(ctx, url, path, size)
	if err != nil {
		return err
	}
	if status == http.StatusRequestEntityTooLarge && size > 0 {
		p.emit("publish", fmt.Sprintf("The store refused %d bytes (HTTP 413); uploading in chunked form", size))
		status, err = p.put(ctx, url, path, -1)
		if err != nil {
			return err
		}
	}
	return putResult(status)
}

// put sends the file at path as the body of a PUT; a negative size asks for a
// chunked upload.
func (p *publisher) put(ctx context.Context, url, path string, size int64) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return 0, err
	}
	req.ContentLength = size
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("the store answered HTTP %d: %s", resp.StatusCode, oneLineOf(string(body)))
	}
	return resp.StatusCode, nil
}

// putResult turns the status of an upload into an error, or nil.
func putResult(status int) error {
	if status/100 == 2 {
		return nil
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
			for arch := range wantArches {
				if section == "images" && version == rec.Version && arch == rec.Arch {
					continue
				}
				haveArches, _ := have[version].(map[string]any)
				if haveArches[arch] == nil {
					return fmt.Errorf("the index lost the %s entry %s %s while this publish ran", section, version, arch)
				}
			}
		}
	}
	return nil
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
