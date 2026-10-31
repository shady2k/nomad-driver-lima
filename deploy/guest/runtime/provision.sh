#!/bin/sh
# Image/guest-only Ubuntu 24.04 provisioning. Never run on an operator host.
set -eu
case "$(uname -m)" in
  aarch64)
    prime_arch=arm64; node_arch=arm64
    prime_sha=5af68c10a088538fd4990fd18778d8c7fe28c0491083dc069c5177f733770e91
    node_sha=dbabf7a1cbf947e6059ffad2c27b80041db97dea1e553b62afdee7f13fca336e
    uv_url=https://files.pythonhosted.org/packages/4c/92/d83d019c63c1ccc6ff17df2bfe76270412f9766e7e71aa4d4ccbf018683f/uv-0.13.5-py3-none-manylinux_2_28_aarch64.whl
    uv_sha=91e58e304b63b6c91bac5561459ab0c3ea24f17bc01ea9359d69fed72142978f
    ;;
  x86_64)
    prime_arch=x64; node_arch=x64
    prime_sha=21ec219183f4134dd23dce9572102e69456066fe374846525561c776d4d1945c
    node_sha=6c172a9ab6718f7b801fb5a7bd486af2ee5e32758086a04b84fcce2aa77095e6
    uv_url=https://files.pythonhosted.org/packages/3a/ad/3c2a81d66d1839063662e5b6c0002ec31f8ee987dbc5295747627ca6160a/uv-0.13.5-py3-none-manylinux_2_17_x86_64.manylinux2014_x86_64.whl
    uv_sha=fa8d26e08e34c29c0c4e820cf3181757086d547240b4f4d31ff1d8bd810ca946
    ;;
  *) echo 'Unsupported guest architecture' >&2; exit 64 ;;
esac
. /etc/os-release
test "$ID:$VERSION_ID" = ubuntu:24.04
mkdir -p /opt/worker/prime /workspace/repo
cd /opt/worker
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "https://github.com/PrimeIntellect-ai/prime-agent/releases/download/v0.10.1/prime-agent-0.10.1-linux-${prime_arch}.tar.gz" -o prime.tar.gz
printf '%s  prime.tar.gz\n' "$prime_sha" | sha256sum -c -
tar -xzf prime.tar.gz -C prime
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "https://nodejs.org/dist/v24.22.1/node-v24.22.1-linux-${node_arch}.tar.xz" -o node.tar.xz
printf '%s  node.tar.xz\n' "$node_sha" | sha256sum -c -
tar -xJf node.tar.xz
ln -s "/opt/worker/node-v24.22.1-linux-${node_arch}/bin/node" /usr/local/bin/node
ln -s "/opt/worker/node-v24.22.1-linux-${node_arch}/bin/npm" /usr/local/bin/npm
cd /opt/worker/acpx
npm ci --ignore-scripts --no-audit --no-fund
ln -s /opt/worker/acpx/node_modules/.bin/acpx /usr/local/bin/acpx
cd /opt/worker
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "$uv_url" -o uv.whl
printf '%s  uv.whl\n' "$uv_sha" | sha256sum -c -
# pip requires the original wheel filename for compatibility validation.
uv_wheel=${uv_url##*/}; mv uv.whl "$uv_wheel"
python3 -m venv /opt/worker/uv-env
/opt/worker/uv-env/bin/pip install --no-index --no-deps "$uv_wheel"
ln -s /opt/worker/uv-env/bin/uv /usr/local/bin/uv
ln -s /opt/worker/prime/prime-agent /usr/local/bin/prime-agent
cp /opt/worker/assets/uv.lock /opt/worker/prime/prime-agent-runtime/uv.lock
cd /opt/worker/prime/prime-agent-runtime
uv sync --python /usr/bin/python3 --frozen --no-dev
prime-agent --version
acpx --version
