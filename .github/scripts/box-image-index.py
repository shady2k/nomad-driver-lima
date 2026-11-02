#!/usr/bin/env python3
"""Merge one built box image into an index.json, for a GitHub release.

    .github/scripts/box-image-index.py --version 1.0.3 --arch amd64 \
        --manifest out/box-base-1.0.3-amd64.json \
        --base-url https://github.com/OWNER/REPO/releases/download/v1.0.3 \
        --index previous-index.json --out index.json

The document is the one `box images` reads and `images/publish.sh` writes
(schema 1):

    {"schema": 1, "updated_at": "...", "images": {
        "<version>": {"<arch>": {"url", "sha256", "sha256_url", "size",
                                 "built_at", "manifest_url", "disk",
                                 "contents"}}}}

Versions are newest first, so the file reads as a history. Without --index the
document starts empty; an entry that is already there is only kept if it names
the same sha256, because a published image is immutable.
"""

from __future__ import annotations

import argparse
import datetime
import json
import sys

ARTIFACTS = {
    "image": "box-base-{version}-{arch}.qcow2",
    "sha256": "box-base-{version}-{arch}.qcow2.sha256",
    "manifest": "box-base-{version}-{arch}.json",
}


def die(message: str) -> None:
    print(f"box-image-index: {message}", file=sys.stderr)
    raise SystemExit(1)


def version_key(text: str):
    """Sort versions numerically, newest-version-first ordering in mind."""
    parts = []
    for chunk in text.replace("-", ".").replace("_", ".").split("."):
        parts.append((0, int(chunk), "") if chunk.isdigit() else (1, 0, chunk))
    return parts


def load_index(path: str) -> dict:
    try:
        with open(path, "rb") as handle:
            raw = handle.read()
    except OSError as err:
        die(f"cannot read {path}: {err}")
    try:
        doc = json.loads(raw)
    except ValueError as err:
        die(f"{path} is not JSON: {err}")
    if not isinstance(doc, dict) or not isinstance(doc.get("images"), dict):
        die(f"{path} is not an image index (no images object)")
    return doc


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--arch", required=True, choices=["amd64", "arm64"])
    parser.add_argument("--manifest", required=True, help="the build manifest of this image")
    parser.add_argument("--base-url", required=True, help="the URL the three files are published under")
    parser.add_argument("--index", help="the index to merge into")
    parser.add_argument("--out", required=True, help="where to write the merged index")
    args = parser.parse_args()

    version, arch = args.version, args.arch
    try:
        with open(args.manifest, "rb") as handle:
            build = json.load(handle)
    except ValueError as err:
        die(f"{args.manifest} is not JSON: {err}")
    except OSError as err:
        die(f"cannot read {args.manifest}: {err}")
    for key in ("sha256", "size", "file"):
        if key not in build:
            die(f"{args.manifest} has no {key}")

    base = args.base_url.rstrip("/")
    entry = {
        "url": f"{base}/{ARTIFACTS['image'].format(version=version, arch=arch)}",
        "sha256": build["sha256"],
        "sha256_url": f"{base}/{ARTIFACTS['sha256'].format(version=version, arch=arch)}",
        "size": build["size"],
        "built_at": build.get("built_at", ""),
        "manifest_url": f"{base}/{ARTIFACTS['manifest'].format(version=version, arch=arch)}",
        "disk": build.get("disk", {}),
        "contents": {
            "base_image": (build.get("base_image") or {}).get("name", ""),
            "kernel": build.get("kernel", ""),
            "tools": build.get("tools", {}),
            "sources": build.get("sources", {}),
            "gaps": build.get("gaps", []),
        },
    }

    doc = {"schema": 1, "images": {}}
    if args.index:
        doc = load_index(args.index)
    doc["schema"] = doc.get("schema", 1)

    before = (doc["images"].get(version) or {}).get(arch)
    if before is not None and before.get("sha256") != entry["sha256"]:
        die(
            f"the index already lists {version} {arch} with sha256 "
            f"{before.get('sha256')}; a published image is immutable, build a new version"
        )

    doc["images"].setdefault(version, {})[arch] = entry
    doc["images"] = {
        name: doc["images"][name]
        for name in sorted(doc["images"], key=version_key, reverse=True)
    }
    doc["updated_at"] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    with open(args.out, "w", encoding="utf-8") as handle:
        json.dump(doc, handle, indent=2, sort_keys=False)
        handle.write("\n")

    print(f"{args.out}: {version} {arch}, sha256 {entry['sha256'][:16]}..., "
          f"{len(doc['images'])} version(s)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
