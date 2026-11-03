#!/usr/bin/env bash
#
# deploy/executors-from-prime.sh -- stage the executor files a box needs, with
# the provider credential read from Vault by Nomad and never from any host.
#
# A box runs prime-agent and opencode inside the guest. Their configuration does
# not belong in the image and must not be in the box job, so it travels as one
# Nomad Variable (nomad/jobs/box-executors) and is copied into
# /run/box/secrets/<executor>/ in the guest (a tmpfs). This script builds the
# directory that `box executors set --from-dir` reads:
#
#   OUT/prime/auth.json.tmpl    ONE provider entry whose key is a Vault template
#   OUT/prime/models.json       ONE provider definition: baseUrl, api, models
#   OUT/prime/settings.json     default provider/model; `packages` only when
#                               --package names one
#   OUT/opencode/opencode.json  the same provider for opencode
#   OUT/opencode/env.tmpl       its key as KEY=<Vault template>
#
# THE KEY IS NEVER STAGED. A `.tmpl` item is not a file: `box executors set`
# stores its text in the Variable, `box create` embeds that text in the box job,
# and Nomad renders it on the client with the Vault token of the task's workload
# identity. So the key is read from Vault at render time and reaches only the
# box's tmpfs -- it is not in this host's files, not in the Variable, not in the
# job spec, and not in this staging directory. That is why the file is named
# auth.json.tmpl: an item with the plain name would have to carry the key itself.
#
# Nothing is read from this host except its models.json definition of the
# provider (and its settings.json, which says what the box's default is not yet).
# The one secret this script names is the Vault path, so it needs no login here.
#
# Only the chosen provider is staged. models.json on this host holds every
# provider definition, and the other providers have no reason to reach the
# cluster, so the file is reduced to --provider here (and `box executors set`
# reduces it again, so a hand-made directory cannot smuggle the rest in).
#
# Why models.json and not the provider extension: prime-agent 0.10.2 (the
# version in the box image) does not register a provider from a git package
# extension -- with this host's own ~/.prime/agent it answers `Unknown provider
# "<provider>"`, while the same provider described in models.json works on
# 0.10.2 and on 0.9.8. The provider definition is read from models.json by both
# versions, and it is a few lines instead of a whole checkout (a Nomad variable
# is limited to 64 KiB in total).
#
# Nothing here prints a credential, because there is none here to print: the only
# secret named is a path. What is printed is the shape -- which provider, which
# Vault path, and which models.
#
# `packages` is where prime-agent is told which packages to load, and a git
# package cannot travel in a 64 KiB variable, so it is dropped by default. Files
# the box image carries instead -- images/build.sh installs IMAGE_ADDONS at
# /opt/box/addons -- are named with --package, and each value is written as it
# is: it is a path inside the box, not on this host, so the image has to have put
# something there.
#
# The Vault side, once per cluster (see deploy/README.md): a JWT auth backend fed
# by Nomad's JWKS URL, a role for the box jobs whose policy reads only the box
# paths, and the key stored from stdin so it is never in a file or an argv:
#
#   bao kv put kv/box/llm key=-
#
# Usage:
#   deploy/executors-from-prime.sh --provider NAME --base-url URL \
#       --vault-secret PATH [options]
#
#   --out DIR            staging directory (default: a new mktemp -d)
#   --prime-dir DIR      prime-agent config directory
#                        (default: $PRIME_AGENT_CODING_AGENT_DIR, else ~/.prime/agent)
#   --provider NAME      the provider id, e.g. myprovider
#   --base-url URL       its OpenAI-compatible base URL, e.g. https://llm.example/v1
#   --vault-secret PATH  the KV v2 API path that holds the key, e.g.
#                        kv/data/box/llm (required: the box renders it, this
#                        script never reads it)
#   --vault-field NAME   the field of that path that holds the key (default: key)
#   --model ID           a model of that provider; repeatable. Without it: the
#                        models of this host's own definition of the provider in
#                        models.json, else a usage error that asks for --model.
#   --package PATH       a path INSIDE the box that prime-agent loads a package
#                        from, e.g. /opt/box/addons/foo; repeatable, and each
#                        one must be absolute. The values become the `packages`
#                        list of settings.json. Without it: no `packages` key at
#                        all, which is what a box with no add-ons needs.
#   --no-opencode        stage prime only
#
# Then, where the CLI can reach the cluster:
#   box executors set --from-dir "$OUT" --provider NAME
#   box executors show
#   box create dev1 --image 1.0.1 ... --vault-role box-workloads
set -euo pipefail

PROG=${0##*/}
die() { printf '%s: %s\n' "$PROG" "$*" >&2; exit 2; }
log() { printf '%s: %s\n' "$PROG" "$*" >&2; }

prime_dir=${PRIME_AGENT_CODING_AGENT_DIR:-$HOME/.prime/agent}
out=''
provider=''
base_url=''
vault_secret=''
vault_field=key
with_opencode=true
models=()
packages=()

while (($# > 0)); do
  case $1 in
    --out) (($# >= 2)) || die "--out needs a directory"; out=$2; shift 2 ;;
    --prime-dir) (($# >= 2)) || die "--prime-dir needs a directory"; prime_dir=$2; shift 2 ;;
    --provider) (($# >= 2)) || die "--provider needs a name"; provider=$2; shift 2 ;;
    --base-url) (($# >= 2)) || die "--base-url needs a URL"; base_url=$2; shift 2 ;;
    --vault-secret) (($# >= 2)) || die "--vault-secret needs a path"; vault_secret=$2; shift 2 ;;
    --vault-field) (($# >= 2)) || die "--vault-field needs a field name"; vault_field=$2; shift 2 ;;
    --model) (($# >= 2)) || die "--model needs a model id"; models+=("$2"); shift 2 ;;
    --package)
      (($# >= 2)) || die "--package needs a path"
      [[ $2 == /* ]] || die "--package '$2' is not an absolute path: it is a path inside the box, e.g. /opt/box/addons/foo"
      packages+=("$2")
      shift 2
      ;;
    --no-opencode) with_opencode=false; shift ;;
    -h | --help) sed -n '2,87p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }
need python3
[[ -n $provider ]] || die "--provider is required (the provider id to configure in the box)"
[[ -n $base_url ]] || die "--base-url is required (that provider's OpenAI-compatible base URL)"
[[ -n $vault_secret ]] || die "--vault-secret is required (the KV v2 API path of the key, e.g. kv/data/box/llm)"
# The path and the field go into Nomad template text: a quote would end the
# string the secret call is read from, and }} would end the call itself.
[[ $vault_secret != *'"'* ]] || die "--vault-secret must not contain a double quote: it goes into a template string"
[[ $vault_secret != *'}}'* ]] || die "--vault-secret must not contain }}: it would close the template call"
[[ $vault_secret != *'{{'* ]] || die "--vault-secret must not contain {{: it would open a second template call"
[[ $vault_field =~ ^[A-Za-z0-9_-]+$ ]] || die "--vault-field '$vault_field' is not a field name: use letters, digits, _ and -"
[[ -d $prime_dir ]] || die "no prime-agent configuration in $prime_dir"
[[ -f $prime_dir/settings.json ]] || die "$prime_dir/settings.json is missing: is this a prime-agent config directory?"

if [[ -z $out ]]; then
  out=$(mktemp -d "${TMPDIR:-/tmp}/box-executors-XXXXXX")
fi
mkdir -p "$out/prime"
chmod 0700 "$out"

# --- auth.json.tmpl, models.json, settings.json, opencode ----------------------
# python writes every document and never prints a value: this host has none to
# print. The one credential reference is the Vault path, which is not a secret.
python3 - "$prime_dir" "$out" "$provider" "$base_url" "$vault_secret" "$vault_field" \
  "$with_opencode" "${packages[@]}" -- "${models[@]}" <<'PY'
import json, os, re, sys

prime_dir, out, provider, base_url, vault_secret, vault_field, with_opencode = sys.argv[1:8]
# The two lists travel in one argument vector, split by the "--" the shell puts
# between them: a package path and a model id are both free-form strings, and
# without a marker one list cannot be told from the other.
rest = sys.argv[8:]
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


# The provider id is a JSON key in auth.json.tmpl and part of the model id in
# settings.json and opencode.json, so it may not carry quotes or spaces.
if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", provider):
    fail(f"--provider {provider!r} is not a provider id: use letters, digits, '.', '_' and '-'")

env_name = re.sub(r"[^A-Z0-9]+", "_", provider.upper()).strip("_") + "_API_KEY"

# --- the one credential reference this box gets -------------------------------
# The templates are Nomad template text, read by consul-template's `secret` on
# the client with the box's own Vault token: .Data.data.<field> is a KV v2
# secret, and `toJSON` quotes and escapes the value so the rendered file is JSON.
opening = '{{ with secret "' + vault_secret + '" }}'
closing = "{{ end }}"
json_value = "{{ .Data.data." + vault_field + " | toJSON }}"
plain_value = "{{ .Data.data." + vault_field + " }}"

auth_out = os.path.join(out, "prime", "auth.json.tmpl")
with open(auth_out, "w") as f:
    f.write('{"' + provider + '": {"type": "api_key", "key": ' + opening + json_value + closing + "}}\n")
os.chmod(auth_out, 0o600)
print(f"prime        auth.json.tmpl: one api_key entry for {provider}, read from {vault_secret}.{vault_field}")

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
    # Nothing on this host describes the provider, and its own model list cannot
    # be asked for without the key -- which this script deliberately does not
    # have. Say what to pass instead of inventing a model.
    fail(f"no model for {provider}: {source_models} has no definition of it, and this script does not "
         f"read the key from anywhere; pass --model ID (one or more)")

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
# The environment variable is only the fallback: the box gets the key as
# prime/auth.json, rendered from Vault before the executor starts.
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
    # opencode reads its key from this variable, which the template fills in the
    # guest: the file holds `ENV_NAME={{ with secret ... }}`, never the key.
    with open(os.path.join(opencode_dir, "env.tmpl"), "w") as f:
        f.write(env_name + "=" + opening + plain_value + closing + "\n")
    os.chmod(os.path.join(opencode_dir, "env.tmpl"), 0o600)
    print(f"opencode     opencode.json + env.tmpl ({env_name}) for {provider}, one provider")
PY

if ! $with_opencode; then
  rm -rf "$out/opencode"
fi

# Only the files this box needs may be in the staging directory: a leftover of an
# earlier run, or a credential copied by hand, is refused rather than shipped.
expected=(prime/auth.json.tmpl prime/models.json prime/settings.json)
if $with_opencode; then
  expected+=(opencode/opencode.json opencode/env.tmpl)
fi
unexpected=0
while IFS= read -r file; do
  rel=${file#"$out"/}
  for want in "${expected[@]}"; do
    [[ $rel == "$want" ]] && continue 2
  done
  log "not expected in $out: $rel"
  unexpected=$((unexpected + 1))
done < <(find "$out" -type f | sort)
if [[ $unexpected -gt 0 ]]; then
  die "$out holds $unexpected file(s) this staging does not write: remove them (or use a new --out directory) " \
    "and run again -- a credential must never be staged by hand, the box reads it from Vault"
fi

log "staged $(find "$out" -type f | wc -l) file(s) in $out:"
find "$out" -type f | sort | while IFS= read -r file; do
  printf '  %-40s %6s bytes\n' "${file#"$out"/}" "$(wc -c <"$file")" >&2
done

printf '\nnext:\n  box executors set --from-dir %q --provider %q\n  box executors show\n  rm -rf %q\n\n' \
  "$out" "$provider" "$out" >&2
printf 'the box needs a Vault role that can read %s: pass --vault-role to %s\n' "$vault_secret" 'box create' >&2
printf '%s\n' "$out"
