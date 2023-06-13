#!/bin/bash
set -e

BINARYEN_VERS=110
BINARYEN_DWN="https://github.com/WebAssembly/binaryen/releases/download/version_${BINARYEN_VERS}/binaryen-version_${BINARYEN_VERS}-x86_64-linux.tar.gz"

WASMOPT_VERS="110"
RUSTC_VERS="1.69.0"

# Install wasm-opt binary
if ! which wasm-opt; then
  curl -OL $BINARYEN_DWN
  tar xf binaryen-version_${BINARYEN_VERS}-x86_64-linux.tar.gz -C /tmp
  rm -f binaryen-version_*.tar.gz
  export PATH=$PATH:/tmp/binaryen-version_${BINARYEN_VERS}/bin
fi

# Check toolchain version
CUR_WASMOPT_VERS=$(wasm-opt --version | awk '{print $3}')
CUR_RUSTC_VERS=$(rustc -V | awk '{print $2}')

if [ "$CUR_RUSTC_VERS" != "$RUSTC_VERS" ] || [ "$CUR_WASMOPT_VERS" != "$WASMOPT_VERS" ]; then   
  echo -e "\n ** Warning: The required versions for Rust and wasm-opt are ${RUSTC_VERS} and ${WASMOPT_VERS}, respectively. Building with different versions may result in failure.\n"
fi

mkdir -p artifacts
cargo fmt --all
cargo clippy --fix --allow-dirty
cargo clean

rustup target add wasm32-unknown-unknown
cargo install cosmwasm-check


RUSTFLAGS='-C link-arg=-s' cargo build --workspace --exclude test-utils --release --lib --target wasm32-unknown-unknown
for WASM in ./target/wasm32-unknown-unknown/release/*.wasm; do
  NAME=$(basename "$WASM" .wasm)${SUFFIX}.wasm
  echo "Creating intermediate hash for $NAME ..."
  echo "$WASM" | openssl sha256 |  echo "Optimizing $NAME ..."
  wasm-opt -Oz "$WASM" -o "artifacts/$NAME"
done

# check all generated wasm files
cosmwasm-check artifacts/cw_ibc_core.wasm
cosmwasm-check artifacts/cw_icon_light_client.wasm
cosmwasm-check artifacts/cw_mock_dapp.wasm
cosmwasm-check artifacts/cw_xcall.wasm
cosmwasm-check artifacts/cw_xcall_ibc_connection.wasm
cosmwasm-check artifacts/cw_xcall_app.wasm
