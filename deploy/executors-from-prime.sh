#!/usr/bin/env bash
#
# deploy/executors-from-prime.sh -- stage the executor files a box needs, from
# this host's own prime-agent configuration.
#
# A box runs prime-agent and opencode inside the guest. Their credentials and
# configuration do not belong in the image and must not be in the box job, so
# they travel as one Nomad Variable (nomad/jobs/box-executors) and are copied
# into /run/box/secrets/<executor>/ in the guest (a tmpfs). This script builds the
# directory that `box executors set --from-dir` reads:
#
#   OUT/prime/auth.json        ONE credential: {"<provider>": <its entry>}
#   OUT/prime/models.json      ONE provider definition: baseUrl, api, models
#   OUT/prime/settings.json    default provider/model; `packages` only when
#                              --package names one
#   OUT/opencode/opencode.json the same provider for opencode
#   OUT/opencode/env           its credential as KEY=VALUE
#
# Only the chosen provider is staged. This host's ~/.prime/agent/auth.json holds
# one entry per provider it ever logged in to, and models.json holds every
# provider definition; neither the whole auth.json nor the other providers have
# any reason to reach the cluster, so both files are reduced to --provider here
# (and `box executors set` reduces them again, so a hand-made directory cannot
# smuggle the rest in).
#
# Why models.json and not the provider extension: prime-agent 0.10.2 (the
# version in the box image) does not register a provider from a git package
# extension -- with this host's own ~/.prime/agent it answers `Unknown provider
# "<provider>"`, while the same provider described in models.json works on
# 0.10.2 and on 0.9.8. The provider definition is read from models.json by both
# versions, and it is a few lines instead of a whole checkout (a Nomad variable
# is limited to 64 KiB in total).
#
# Nothing here prints, echoes or logs a token: the values only ever go from the
# source file into the staging directory (mode 0600) and from there into Nomad.
# What is printed is the shape: which provider, and which key names its entry
# has (`access`, `refresh`, `expires`, ...).
#
# `packages` is where prime-agent is told which packages to load, and a git
# package cannot travel in a 64 KiB variable, so it is dropped by default. Files
# the box image carries instead -- images/build.sh installs IMAGE_ADDONS at
# /opt/box/addons -- are named with --package, and each value is written as it
# is: it is a path inside the box, not on this host, so the image has to have put
# something there.
#
# Usage:
#   deploy/executors-from-prime.sh --provider NAME --base-url URL [options]
#
#   --out DIR            staging directory (default: a new mktemp -d)
#   --prime-dir DIR      prime-agent config directory
#                        (default: $PRIME_AGENT_CODING_AGENT_DIR, else ~/.prime/agent)
#   --provider NAME      the provider id, e.g. myprovider (its credential
#                        must be in auth.json: run `prime-agent` and /login once)
#   --base-url URL       its OpenAI-compatible base URL, e.g. https://llm.example/v1
#   --model ID           a model of that provider; repeatable. Without it: the
#                        models of this host's own definition of the provider
#                        in models.json, else <base-url>/models asked with the
#                        credential.
#   --package PATH       a path INSIDE the box that prime-agent loads a package
#                        from, e.g. /opt/box/addons/foo; repeatable, and each
#                        one must be absolute. The values become the `packages`
#                        list of settings.json. Without it: no `packages` key at
#                        all, which is what a box with no add-ons needs.
#   --no-opencode        stage prime only
#
# Then:
#   box executors set --from-dir "$OUT" --provider NAME
#   box executors show
#   box create dev1 --image 1.0.1 ...
set -euo pipefail

PROG=${0##*/}
die() { printf '%s: %s\n' "$PROG" "$*" >&2; exit 2; }
log() { printf '%s: %s\n' "$PROG" "$*" >&2; }

prime_dir=${PRIME_AGENT_CODING_AGENT_DIR:-$HOME/.prime/agent}
out=''
provider=''
base_url=''
with_opencode=true
models=()
packages=()

while (($# > 0)); do
  case $1 in
    --out) (($# >= 2)) || die "--out needs a directory"; out=$2; shift 2 ;;
    --prime-dir) (($# >= 2)) || die "--prime-dir needs a directory"; prime_dir=$2; shift 2 ;;
    --provider) (($# >= 2)) || die "--provider needs a name"; provider=$2; shift 2 ;;
    --base-url) (($# >= 2)) || die "--base-url needs a URL"; base_url=$2; shift 2 ;;
    --model) (($# >= 2)) || die "--model needs a model id"; models+=("$2"); shift 2 ;;
    --package)
      (($# >= 2)) || die "--package needs a path"
      [[ $2 == /* ]] || die "--package '$2' is not an absolute path: it is a path inside the box, e.g. /opt/box/addons/foo"
      packages+=("$2")
      shift 2
      ;;
    --no-opencode) with_opencode=false; shift ;;
    -h | --help) sed -n '2,69p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }
need python3
[[ -n $provider ]] || die "--provider is required (the provider id to configure in the box)"
[[ -n $base_url ]] || die "--base-url is required (that provider's OpenAI-compatible base URL)"
[[ -d $prime_dir ]] || die "no prime-agent configuration in $prime_dir"
[[ -f $prime_dir/settings.json ]] || die "$prime_dir/settings.json is missing: is this a prime-agent config directory?"
[[ -f $prime_dir/auth.json ]] || die "$prime_dir/auth.json is missing: run prime-agent and /login $provider once"

if [[ -z $out ]]; then
  out=$(mktemp -d "${TMPDIR:-/tmp}/box-executors-XXXXXX")
fi
mkdir -p "$out/prime"
chmod 0700 "$out"

# --- auth.json, models.json, settings.json, opencode ---------------------------
# python writes every document and never prints a value: the only output is the
# provider and the key names of the reduced files.
python3 - "$prime_dir" "$out" "$provider" "$base_url" "$with_opencode" "${packages[@]}" -- "${models[@]}" <<'PY'
import json, os, re, sys, urllib.request

prime_dir, out, provider, base_url, with_opencode = sys.argv[1:6]
# The two lists travel in one argument vector, split by the "--" the shell puts
# between them: a package path and a model id are both free-form strings, and
# without a marker one list cannot be told from the other.
rest = sys.argv[6:]
if "--" in rest:
    cut = rest.index("--")
    packages, wanted_models = rest[:cut], rest[cut + 1:]
else:
    packages, wanted_models = [], rest
with_opencode = with_opencode == "true"


def fail(message):
    raise SystemExit(message)


def keys_of(value):
    return sorted(value) if isinstance(value, dict) else []


# --- the one credential this box gets --------------------------------------
auth_path = os.path.join(prime_dir, "auth.json")
try:
    auth = json.load(open(auth_path))
except ValueError as err:
    fail(f"{auth_path} is not valid JSON: {err}")
credential = auth.get(provider)
if not isinstance(credential, dict):
    fail(f"auth.json has no credential for {provider}: it holds {', '.join(keys_of(auth)) or 'nothing'}; run prime-agent and /login {provider}")
token = credential.get("access") or credential.get("key")
if not token:
    fail(f"the {provider} credential has no access token or key")

auth_out = os.path.join(out, "prime", "auth.json")
with open(auth_out, "w") as f:
    json.dump({provider: credential}, f, indent=2)
    f.write("\n")
os.chmod(auth_out, 0o600)
print(f"prime        auth.json: one credential for {provider} ({', '.join(keys_of(credential))})")

env_name = re.sub(r"[^A-Z0-9]+", "_", provider.upper()).strip("_") + "_API_KEY"

# This host's own definition of the provider, when it has one: its models carry
# what prime-agent needs to use them well (reasoning, input, contextWindow,
# maxTokens, cost, compat), and the provider may carry compat and headers.
source_models = os.path.join(prime_dir, "models.json")
doc = {"providers": {}}
if os.path.isfile(source_models):
    try:
        doc = json.load(open(source_models))
    except ValueError:
        doc = {"providers": {}}
if not isinstance(doc, dict):
    doc = {"providers": {}}
source_def = (doc.get("providers") or {}).get(provider) if isinstance(doc.get("providers"), dict) else None
if not isinstance(source_def, dict):
    source_def = {}
source_entries = {m["id"]: m for m in source_def.get("models") or [] if isinstance(m, dict) and m.get("id")}

if wanted_models:
    model_ids = wanted_models
elif source_entries:
    model_ids = list(source_entries)
else:
    # Ask the provider which models it serves, with the credential, and never
    # print the answer's headers.
    request = urllib.request.Request(
        base_url.rstrip("/") + "/models",
        headers={"Accept": "application/json", "Authorization": f"Bearer {token}"},
    )
    with urllib.request.urlopen(request, timeout=20) as response:
        payload = json.load(response)
    model_ids = [m["id"] for m in payload.get("data", []) if isinstance(m, dict) and m.get("id")]
    if not model_ids:
        fail(f"{base_url}/models listed no model ids; pass --model explicitly")

# The provider definition prime-agent reads (0.9.8 and 0.10.2). The rest of the
# source document (schema and so on) is kept; every other provider is dropped.
# This host's definition is kept whole, with its models' fields; only the base
# URL and the key's variable are set here, and a model it does not describe
# gets an id and a name.
models_path = os.path.join(out, "prime", "models.json")
dropped = [name for name in keys_of(doc.get("providers")) if name != provider]
definition = dict(source_def)
definition.setdefault("api", "openai-completions")
definition.setdefault("authHeader", True)
definition["baseUrl"] = base_url
definition["apiKey"] = env_name
definition["models"] = [source_entries.get(model, {"id": model, "name": model}) for model in model_ids]
doc["providers"] = {provider: definition}
described = sum(1 for model in model_ids if model in source_entries)
with open(models_path, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
os.chmod(models_path, 0o600)
print(f"prime        models.json: {provider} at {base_url}, {len(model_ids)} model(s), "
      f"{described} with this host's own description"
      + (f"; dropped the definition(s) of {', '.join(dropped)}" if dropped else ""))

# settings.json without `packages`: a git package cannot travel in a 64 KiB
# variable, and this box gets its provider from models.json. What the image does
# carry -- files at /opt/box/addons -- is named here, and each path is written as
# it is given: it is the path the guest sees, not one on this host.
settings = json.load(open(os.path.join(prime_dir, "settings.json")))
settings.pop("packages", None)
if packages:
    settings["packages"] = packages
settings["defaultProvider"] = provider
settings["defaultModel"] = model_ids[0]
settings_path = os.path.join(out, "prime", "settings.json")
with open(settings_path, "w") as f:
    json.dump(settings, f, indent=2)
    f.write("\n")
os.chmod(settings_path, 0o600)
print(f"prime        settings.json: default {provider}/{model_ids[0]}"
      + (f", {len(packages)} package(s) at {', '.join(packages)}" if packages else ""))

if with_opencode:
    opencode_dir = os.path.join(out, "opencode")
    os.makedirs(opencode_dir, exist_ok=True)
    config = {
        "$schema": "https://opencode.ai/config.json",
        "model": f"{provider}/{model_ids[0]}",
        "providers": {
            provider: {
                "name": provider,
                "env": [env_name],
                "package": "@opencode/ai/providers/openai-compatible",
                "settings": {"baseURL": base_url},
                "models": {model: {"name": model} for model in model_ids},
            }
        },
    }
    with open(os.path.join(opencode_dir, "opencode.json"), "w") as f:
        json.dump(config, f, indent=2)
        f.write("\n")
    os.chmod(os.path.join(opencode_dir, "opencode.json"), 0o600)
    with open(os.path.join(opencode_dir, "env"), "w") as f:
        f.write(f"{env_name}={token}\n")
    os.chmod(os.path.join(opencode_dir, "env"), 0o600)
    print(f"opencode     opencode.json + env ({env_name}) for {provider}, one provider")
PY

if ! $with_opencode; then
  rm -rf "$out/opencode"
fi

# Only the files this box needs may be in the staging directory; a leftover from
# an earlier run (or a whole auth.json copied by hand) is refused, not shipped.
log "staged $(find "$out" -type f | wc -l) file(s) in $out:"
find "$out" -type f | sort | while IFS= read -r file; do
  printf '  %-40s %6s bytes\n' "${file#"$out"/}" "$(wc -c <"$file")" >&2
done

printf '\nnext:\n  box executors set --from-dir %q --provider %q\n  box executors show\n  rm -rf %q\n\n' \
  "$out" "$provider" "$out" >&2
printf '%s\n' "$out"
