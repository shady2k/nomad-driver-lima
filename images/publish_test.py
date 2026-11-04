#!/usr/bin/env python3
"""images/publish_test.py -- test images/publish.sh against a local fake store.

Nothing real is ever touched. A stdlib HTTP server plays the store: PUT, GET,
HEAD, a body-size limit that answers 413 (like a reverse proxy in front of one),
a credential header check, and no DELETE (a write-only credential cannot take an
upload back). The token comes from the environment, in the two ways publish.sh
reads it: a value, and a file holding it.

Run it from anywhere:

    python3 images/publish_test.py

Exit code 0 means every case behaved as specified.
"""

import hashlib
import http.server
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
PUBLISH = os.path.join(HERE, "publish.sh")
# A platform bundle is published from its archive; the test builds small ones
# with the same shape images/platform.sh writes (manifest.json and bin/boxd).
BUNDLE_BOXD = "1.2.3"
BUNDLE_TOOLS = {"prime-agent": "0.10.2", "opencode": "2.2.5"}
# The store's root is the URL itself; the layout publish.sh writes sits below it.
BASE = "/box-base"
API_KEY = "test-key-0123456789"
BODY_LIMIT = 400_000  # the store's body-size limit, for the 413 case


class Repo:
    """One fake store."""

    def __init__(self, body_limit=BODY_LIMIT, header="Authorization", api_key=API_KEY):
        self.store = {}
        self.body_limit = body_limit
        # header is the header publish.sh sends the token in, api_key the token
        # itself; "Bearer <token>" is accepted too, which is the default scheme.
        self.header = header
        self.api_key = api_key
        self.fail_put = set()      # paths answered with 500
        self.rewrite_index = None  # a callable(raw bytes) -> bytes for index.json
        self.lock = threading.Lock()
        self.puts = []

    def get(self, path):
        with self.lock:
            return self.store.get(path)

    def put(self, path, data):
        with self.lock:
            if path in self.fail_put:
                raise PermissionError(path)
            if path.endswith("index.json") and self.rewrite_index is not None:
                data = self.rewrite_index(data)
            self.store[path] = data
        return data


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # keep the test output readable
        pass

    def repo(self):
        return self.server.repo

    def url_path(self):
        return urllib.parse.urlparse(self.path).path

    def reply(self, status, body=b"", ctype="application/octet-stream"):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def authenticated(self):
        repo = self.repo()
        if repo.api_key is None:
            return True
        value = self.headers.get(repo.header)
        return value in (repo.api_key, "Bearer " + repo.api_key)

    def read_body(self):
        """The request body, or None when it is larger than the limit."""
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            chunks = []
            while True:
                line = self.rfile.readline().strip()
                if not line:
                    continue
                size = int(line.split(b";")[0], 16)
                if size == 0:
                    self.rfile.readline()
                    break
                chunks.append(self.rfile.read(size))
                self.rfile.read(2)
            return b"".join(chunks)
        length = int(self.headers.get("Content-Length") or 0)
        if length > self.repo().body_limit:
            # Drain and refuse, like the proxy does.
            remaining = length
            while remaining > 0:
                chunk = self.rfile.read(min(65536, remaining))
                if not chunk:
                    break
                remaining -= len(chunk)
            return None
        return self.rfile.read(length)

    def do_HEAD(self):
        path = self.url_path()
        self.send_response(200 if self.repo().get(path) is not None else 404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        path = self.url_path()
        data = self.repo().get(path)
        if data is None:
            self.reply(404, b"not found", "text/plain")
            return
        self.reply(200, data)

    def do_PUT(self):
        path = self.url_path()
        self.repo().puts.append(path)
        if not self.authenticated():
            self.reply(401, b"no credential", "text/plain")
            return
        body = self.read_body()
        if body is None:
            self.reply(413, b"body too large", "text/plain")
            return
        try:
            self.repo().put(path, body)
        except PermissionError:
            self.reply(500, b"server error", "text/plain")
            return
        self.reply(200)

    def do_DELETE(self):
        self.reply(403, b"this key cannot delete", "text/plain")


def start_repo(**kwargs):
    repo = Repo(**kwargs)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.repo = repo
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return repo, server, f"http://127.0.0.1:{server.server_address[1]}"


def digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def sha256sum_is_gnu():
    try:
        out = subprocess.run(["sha256sum", "--version"], capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return False
    return out.returncode == 0


class Case:
    """One publish run against a fresh repository."""

    seq = 0

    def __init__(self, root, version, arch="amd64", size=200_000, repo_kwargs=None):
        Case.seq += 1
        self.root = os.path.join(root, f"case-{Case.seq:02d}-{version}-{arch}")
        os.makedirs(self.root)
        self.version = version
        self.arch = arch
        self.out = os.path.join(self.root, "out")
        os.makedirs(self.out)
        self.image = os.path.join(self.out, f"box-base-{version}-{arch}.qcow2")
        self.manifest = os.path.join(self.out, f"box-base-{version}-{arch}.json")
        self.repo, self.server, self.url = start_repo(**(repo_kwargs or {}))
        self.write_image(size)

    def write_image(self, size, seed=0):
        with open(self.image, "wb") as f:
            f.write(b"\0qcow2" + bytes((i + seed) % 251 for i in range(size)))
        sha = digest(self.image)
        with open(self.image + ".sha256", "w") as f:
            f.write(f"{sha}  {os.path.basename(self.image)}\n")
        with open(self.manifest, "w") as f:
            json.dump({
                "version": self.version, "arch": self.arch, "sha256": sha,
                "size": os.path.getsize(self.image), "built_at": "2026-11-02T10:00:00Z",
                "disk": {"virtual_gib": 8}, "tools": {}, "base_image": {"name": "ubuntu-24.04"},
                "kernel": "6.8", "sources": {}, "gaps": [],
                # An image built for a platform bundle says so, and the index entry
                # of an image has to carry it.
                "platform": "mount",
            }, f)
        return sha

    def sha(self):
        return digest(self.image)

    def run(self, extra_env=None, extra_args=None):
        env = dict(os.environ)
        env.update({
            "OUT": self.out,
            "BOX_STORE_URL": self.url,
            "BOX_STORE_TOKEN": API_KEY,
        })
        for key in ("BOX_STORE_TOKEN_FILE", "BOX_STORE_TOKEN_HEADER", "BOX_STORE_TOKEN_SCHEME"):
            env.pop(key, None)
        env.update(extra_env or {})
        # None removes a variable, so a case can test the absence of one.
        for key, value in list(env.items()):
            if value is None:
                del env[key]
        return subprocess.run(["bash", PUBLISH, *(extra_args or []), self.arch, self.version],
                              capture_output=True, text=True, timeout=300, env=env)

    def file_at(self, prefix, name):
        # A bundle's path is its own top-level directory in the store, not below
        # box-base: /box-platform/<version>/<arch>/<name>.
        return self.repo.get(f"/{prefix}/{self.version}/{self.arch}/{name}")

    def index(self):
        data = self.repo.get(f"{BASE}/index.json")
        return json.loads(data) if data else None

    def lock(self):
        data = self.repo.get(f"{BASE}/.lock")
        return data.decode() if data else None

    def file(self, name):
        return self.repo.get(f"{BASE}/{self.version}/{self.arch}/{name}")

    def close(self):
        self.server.shutdown()
        self.server.server_close()


FAILURES = []


def check(name, condition, detail=""):
    if condition:
        print(f"  ok   {name}")
    else:
        print(f"  FAIL {name}{(': ' + detail) if detail else ''}")
        FAILURES.append(name)


def expect(result, label):
    if result.returncode != 0:
        print(f"  ---  {label} output:\n{result.stdout}{result.stderr}")
    return result


def bundle_archive(root, name_version, arch, manifest_version=None, boxd=BUNDLE_BOXD, tools=None, addons=None):
    """A bundle archive with the shape images/platform.sh writes, for a store test."""
    manifest_version = manifest_version or name_version
    path = os.path.join(root, f"box-platform-{name_version}-linux-{arch}.tar.gz")
    manifest = {
        "schema": 1,
        "version": manifest_version,
        "arch": arch,
        "boxd": boxd,
        "tools": BUNDLE_TOOLS if tools is None else tools,
        "addons": addons,
        "built_at": "2026-11-04T10:00:00Z",
    }
    raw = json.dumps(manifest, indent=2).encode() + b"\n"
    binary = b"\x7fELF not a real boxd, the store does not run it\n"
    with tarfile.open(path, "w:gz") as tf:
        for name, data, mode in (("./manifest.json", raw, 0o644), ("./bin/boxd", binary, 0o755)):
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = mode
            info.mtime = 0
            tf.addfile(info, io.BytesIO(data))
    return path, manifest, raw


def case_fresh(root):
    print("case: a fresh version publishes, verifies and indexes")
    c = Case(root, "1.0.0")
    res = expect(c.run(), "fresh")
    check("exit 0", res.returncode == 0, res.stderr)
    idx = c.index()
    check("index written", idx is not None)
    entry = (idx or {}).get("images", {}).get("1.0.0", {}).get("amd64", {})
    check("index entry sha256 matches", entry.get("sha256") == c.sha(), str(entry)[:200])
    check("index entry size matches", entry.get("size") == os.path.getsize(c.image))
    check("index entry url", entry.get("url", "").endswith(f"/box-base/1.0.0/amd64/{os.path.basename(c.image)}"))
    check("index entry carries the image's platform field", entry.get("platform") == "mount", str(entry)[:200])
    check("index entry records the platform in its contents", (entry.get("contents") or {}).get("platform") == "mount")
    stored = c.file(os.path.basename(c.image))
    check("image stored", stored == open(c.image, "rb").read())
    check("sha256 file stored", c.file(os.path.basename(c.image) + ".sha256") == open(c.image + ".sha256", "rb").read())
    check("manifest stored", c.file(os.path.basename(c.manifest)) == open(c.manifest, "rb").read())
    check("lock released", "state=released" in (c.lock() or ""), str(c.lock()))
    check("no credential in the output", API_KEY not in res.stdout + res.stderr)
    c.close()


def case_immutable(root):
    print("case: a published version is refused, not replaced")
    c = Case(root, "1.0.0")
    expect(c.run(), "first")
    before = c.file(os.path.basename(c.image))
    # A rebuilt image with the same version: it must not reach the repository.
    sha = c.write_image(210_000, seed=7)
    res = expect(c.run(), "second")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("says immutable", "immutable" in res.stderr, res.stderr[-400:])
    check("image unchanged", c.file(os.path.basename(c.image)) == before)
    check("index unchanged", (c.index() or {}).get("images", {}).get("1.0.0", {}).get("amd64", {}).get("sha256") == digest_sha(before))
    check("lock released", "state=released" in (c.lock() or ""))
    c.close()


def digest_sha(data):
    return hashlib.sha256(data).hexdigest()


def case_chunked(root):
    print("case: an image over the proxy limit goes through in chunked form")
    c = Case(root, "1.1.0", size=BODY_LIMIT + 250_000)
    res = expect(c.run(), "chunked")
    check("exit 0", res.returncode == 0, res.stderr)
    check("chunked retry was used", "chunked" in res.stderr, res.stderr[-400:])
    stored = c.file(os.path.basename(c.image))
    check("image stored whole", stored is not None and digest_sha(stored) == c.sha())
    check("index lists it", (c.index() or {}).get("images", {}).get("1.1.0", {}).get("amd64", {}).get("sha256") == c.sha())
    c.close()


def case_merge(root):
    print("case: a second version merges into the index without dropping the first")
    c = Case(root, "1.0.0")
    expect(c.run(), "first")
    first = (c.index() or {}).get("images", {}).get("1.0.0", {}).get("amd64", {}).get("sha256")
    # Another publisher wrote an entry between the two runs.
    idx = c.index()
    idx["images"]["0.9.0"] = {"amd64": {"sha256": "a" * 64, "url": "u", "size": 1, "built_at": "x"}}
    c.repo.store[f"{BASE}/index.json"] = json.dumps(idx).encode()

    second = Case(root, "1.2.0", arch="arm64", size=150_000)
    second.repo = c.repo
    second.url = c.url
    res = expect(second.run(), "second")
    check("exit 0", res.returncode == 0, res.stderr)
    idx = c.index()
    check("first entry kept", idx["images"]["1.0.0"]["amd64"]["sha256"] == first)
    check("unrelated entry kept", idx["images"]["0.9.0"]["amd64"]["sha256"] == "a" * 64)
    check("new entry written", idx["images"]["1.2.0"]["arm64"]["sha256"] == second.sha())
    check("versions are newest first", list(idx["images"]) == sorted(idx["images"], reverse=True), str(list(idx["images"])))
    second.close()
    c.close()


def case_live_lock(root):
    print("case: a live lock stops a second publisher")
    c = Case(root, "1.0.0")
    now = int(time.time())
    c.repo.store[f"{BASE}/.lock"] = (
        f"state=held\nowner=other-host-1234\npid=1234\nversion=1.0.0\narch=amd64\ntime={now}\n").encode()
    res = expect(c.run(), "locked")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("names the holder", "held by other-host-1234" in res.stderr, res.stderr[-400:])
    check("nothing uploaded", c.file(os.path.basename(c.image)) is None)
    check("lock untouched", "other-host-1234" in (c.lock() or ""))
    c.close()


def case_stale_lock(root):
    print("case: a stale lock is taken over")
    c = Case(root, "1.0.0")
    old = int(time.time()) - 31 * 60
    c.repo.store[f"{BASE}/.lock"] = (
        f"state=held\nowner=crashed-host-99\npid=99\nversion=1.0.0\narch=amd64\ntime={old}\n").encode()
    res = expect(c.run(), "stale")
    check("exit 0", res.returncode == 0, res.stderr)
    check("says it took over", "stale lock" in res.stderr, res.stderr[-400:])
    check("lock released by this run", "state=released" in (c.lock() or ""), str(c.lock()))
    check("published", c.file(os.path.basename(c.image)) is not None)
    c.close()


def case_index_tampered(root):
    print("case: an index that does not read back as written fails the run")
    c = Case(root, "1.0.0")

    def drop_entries(raw):
        doc = json.loads(raw)
        doc["images"] = {v: a for v, a in doc["images"].items() if v != "1.0.0"}
        return json.dumps(doc).encode()

    c.repo.rewrite_index = drop_entries
    res = expect(c.run(), "tampered")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("reports the missing entry", "does not list 1.0.0 amd64" in res.stderr, res.stderr[-400:])
    c.close()


def case_upload_failure(root):
    print("case: a failed upload leaves the index alone")
    c = Case(root, "1.0.0")
    c.repo.fail_put = {f"{BASE}/1.0.0/amd64/{os.path.basename(c.image)}"}
    res = expect(c.run(), "upload failure")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("index not written", c.index() is None)
    check("lock released", "state=released" in (c.lock() or ""))
    c.close()


def case_wrong_credential(root):
    print("case: a wrong token is refused by the store")
    c = Case(root, "1.0.0")
    res = expect(c.run({"BOX_STORE_TOKEN": "the-wrong-token"}), "wrong token")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("the store answered 401", "401" in res.stderr, res.stderr[-400:])
    check("nothing reached the index", c.index() is None)
    check("nothing uploaded", c.file(os.path.basename(c.image)) is None)
    check("the token is not printed", "the-wrong-token" not in res.stdout + res.stderr)
    c.close()


def case_token_file(root):
    print("case: the token can come from a file")
    c = Case(root, "1.0.0")
    token_file = os.path.join(c.root, "token")
    with open(token_file, "w") as f:
        f.write(API_KEY + "\n")
    res = expect(c.run({"BOX_STORE_TOKEN": None, "BOX_STORE_TOKEN_FILE": token_file}), "token file")
    check("exit 0", res.returncode == 0, res.stderr)
    check("the token is not printed", API_KEY not in res.stdout + res.stderr)
    check("published", c.file(os.path.basename(c.image)) is not None)
    c.close()


def case_custom_header(root):
    print("case: the header name and scheme are configurable")
    c = Case(root, "1.0.0", repo_kwargs={"header": "X-Api-Key"})
    res = expect(c.run({"BOX_STORE_TOKEN_HEADER": "X-Api-Key", "BOX_STORE_TOKEN_SCHEME": ""}), "custom header")
    check("exit 0", res.returncode == 0, res.stderr)
    check("index lists it", (c.index() or {}).get("images", {}).get("1.0.0", {}).get("amd64", {}).get("sha256") == c.sha())
    c.close()


def case_no_auth(root):
    print("case: a store that needs no token is published to without one")
    c = Case(root, "1.0.0", repo_kwargs={"api_key": None})
    res = expect(c.run({"BOX_STORE_TOKEN": ""}), "no token")
    check("exit 0", res.returncode == 0, res.stderr)
    check("says so", "no token configured" in res.stderr, res.stderr[-400:])
    check("published", c.file(os.path.basename(c.image)) is not None)
    c.close()


def case_platform_fresh(root):
    print("case: a platform bundle publishes into the same store and index")
    c = Case(root, "1.0.0")
    path, manifest, raw = bundle_archive(c.root, "1.0.0", "amd64")
    res = expect(c.run(extra_args=["--platform", path]), "platform")
    check("exit 0", res.returncode == 0, res.stderr)
    idx = c.index() or {}
    entry = (idx.get("platforms") or {}).get("1.0.0", {}).get("amd64", {})
    name = os.path.basename(path)
    check("the bundle's sha256 is the archive's", entry.get("sha256") == digest(path), str(entry)[:200])
    check("the bundle's size is the archive's", entry.get("size") == os.path.getsize(path))
    check("the bundle's url is its own path", entry.get("url", "").endswith(f"/box-platform/1.0.0/amd64/{name}"))
    check("the bundle's boxd is the manifest's", entry.get("boxd") == BUNDLE_BOXD)
    check("the bundle's tools are the manifest's", entry.get("tools") == BUNDLE_TOOLS)
    check("the bundle's built_at is the manifest's", entry.get("built_at") == "2026-11-04T10:00:00Z")
    check("the bundle's entry names its manifest", entry.get("manifest_url", "").endswith(f"/{name}.json"))
    check("the archive is stored", c.file_at("box-platform", name) == open(path, "rb").read())
    check("the .sha256 is stored",
          c.file_at("box-platform", name + ".sha256") == f"{digest(path)}  {name}\n".encode(),
          str(c.file_at("box-platform", name + ".sha256")))
    check("the bundle's manifest is stored", c.file_at("box-platform", name + ".json") == raw)
    check("no image entry was invented", not (idx.get("images") or {}))
    check("lock released", "state=released" in (c.lock() or ""), str(c.lock()))
    check("no credential in the output", API_KEY not in res.stdout + res.stderr)
    c.close()


def case_platform_keeps_images(root):
    print("case: a bundle and an image live in one index without dropping each other")
    c = Case(root, "1.0.0")
    expect(c.run(), "image")
    image_entry = (c.index() or {}).get("images", {}).get("1.0.0", {}).get("amd64", {})
    path, _, _ = bundle_archive(c.root, "1.0.0", "amd64")
    res = expect(c.run(extra_args=["--platform", path]), "platform")
    check("exit 0", res.returncode == 0, res.stderr)
    idx = c.index() or {}
    check("the image entry survived", (idx.get("images") or {}).get("1.0.0", {}).get("amd64", {}).get("sha256") == image_entry.get("sha256"))
    check("the bundle entry is there", (idx.get("platforms") or {}).get("1.0.0", {}).get("amd64", {}).get("sha256") == digest(path))
    c.close()


def case_platform_immutable(root):
    print("case: a published bundle is refused, not replaced")
    c = Case(root, "1.0.0")
    path, _, _ = bundle_archive(c.root, "1.0.0", "amd64")
    expect(c.run(extra_args=["--platform", path]), "first")
    before = c.file_at("box-platform", os.path.basename(path))
    # A rebuilt bundle with the same version and a different digest.
    path2, _, _ = bundle_archive(c.root, "1.0.0", "amd64", boxd="9.9.9")
    res = expect(c.run(extra_args=["--platform", path2]), "second")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("says immutable", "immutable" in res.stderr, res.stderr[-400:])
    check("the stored archive is unchanged", c.file_at("box-platform", os.path.basename(path)) == before)
    c.close()


def case_platform_wrong_name(root):
    print("case: a bundle whose name does not match this version and arch is refused")
    c = Case(root, "1.0.0")
    path, _, _ = bundle_archive(c.root, "0.9.0", "amd64")
    res = expect(c.run(extra_args=["--platform", path]), "wrong name")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("names the file", "box-platform-1.0.0-linux-amd64.tar.gz" in res.stderr, res.stderr[-400:])
    check("nothing uploaded", c.file_at("box-platform", os.path.basename(path)) is None)
    c.close()


def case_platform_manifest_mismatch(root):
    print("case: a bundle whose manifest is not the one its name promises is refused")
    c = Case(root, "1.0.0")
    path, _, _ = bundle_archive(c.root, "1.0.0", "amd64", manifest_version="9.9.9")
    res = expect(c.run(extra_args=["--platform", path]), "manifest mismatch")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("says which version it carries", "9.9.9" in res.stderr, res.stderr[-400:])
    check("nothing reached the index", c.index() is None)
    c.close()


def case_platform_not_a_bundle(root):
    print("case: an archive without a manifest is not a bundle")
    c = Case(root, "1.0.0")
    path = os.path.join(c.root, "box-platform-1.0.0-linux-amd64.tar.gz")
    with tarfile.open(path, "w:gz") as tf:
        info = tarfile.TarInfo("./bin/boxd")
        info.size = 3
        tf.addfile(info, io.BytesIO(b"box"))
    res = expect(c.run(extra_args=["--platform", path]), "not a bundle")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("says it has no manifest", "no manifest.json" in res.stderr, res.stderr[-400:])
    c.close()


def case_missing_url(root):
    print("case: no store URL is a plain error")
    c = Case(root, "1.0.0")
    res = expect(c.run({"BOX_STORE_URL": ""}), "no url")
    check("exit != 0", res.returncode != 0, res.stdout)
    check("names the variable", "BOX_STORE_URL is not set" in res.stderr, res.stderr[-400:])
    check("nothing uploaded", c.file(os.path.basename(c.image)) is None)
    c.close()


def main():
    if not shutil.which("curl"):
        raise SystemExit("curl is required")
    if not sha256sum_is_gnu():
        raise SystemExit("sha256sum is required")

    root = tempfile.mkdtemp(prefix="publish-test-")
    keep = os.environ.get("KEEP") == "1"
    try:
        case_fresh(root)
        case_immutable(root)
        case_chunked(root)
        case_merge(root)
        case_live_lock(root)
        case_stale_lock(root)
        case_index_tampered(root)
        case_upload_failure(root)
        case_wrong_credential(root)
        case_token_file(root)
        case_custom_header(root)
        case_no_auth(root)
        case_platform_fresh(root)
        case_platform_keeps_images(root)
        case_platform_immutable(root)
        case_platform_wrong_name(root)
        case_platform_manifest_mismatch(root)
        case_platform_not_a_bundle(root)
        case_missing_url(root)
    finally:
        if keep:
            print(f"kept {root}")
        else:
            shutil.rmtree(root, ignore_errors=True)

    if FAILURES:
        print(f"\n{len(FAILURES)} check(s) failed: {', '.join(FAILURES)}")
        return 1
    print("\nall publish checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
