#!/usr/bin/env bash
#
# Compile every generated SDK, and skip the ones whose toolchain is not present.
#
# This exists because the drift check cannot see a broken tree: a specification
# change can produce code that matches the spec byte for byte and does not
# compile, and the drift check would pass it. Every language here has a different
# toolchain, so this both compiles what it can and reports what it could not —
# silence from a missing toolchain must not read as success.
#
# It runs in CI (where every toolchain is installed) and on a developer's machine
# with whatever happens to be there. The SDKs are generated and compiled in CI, so
# a local run is a convenience, not the gate.
#
# Usage: hack/sdk-compile.sh
set -euo pipefail

cd "$(dirname "$0")/.."

compiled=()
skipped=()

# have runs a tool's version command and reports whether it actually works.
#
# `command -v` alone is not enough on macOS: /usr/bin/javac exists as a stub that
# prints "Unable to locate a Java Runtime" and exits non-zero when no JDK is
# installed, so a check that only looked for the path would try to compile with a
# javac that cannot run.
have() {
  command -v "$1" >/dev/null 2>&1 && "$@" >/dev/null 2>&1
}

# Go: the generated tree is a module of its own once the generator has written
# its go.mod, so it is built from inside. Its runtime pulls third-party packages
# and must not be folded into this repository's module, which is why the generated
# module file is kept rather than ignored.
if have go version; then
  if [ -f sdk/golang/go.mod ]; then
    (cd sdk/golang && go build ./...)
    # The hand-written helper lives inside this module and is the one part with a
    # test suite, so it is exercised here — nothing else runs it, because the
    # root module does not reach into this directory.
    (cd sdk/golang && go test ./applabext/...)
  fi
  compiled+=("go")
else
  skipped+=("go (no go toolchain)")
fi

# Python: compile to bytecode without importing, so a missing runtime dependency
# in the generated package does not fail the check — only syntax does.
if have python3 --version; then
  python3 -m compileall -q sdk/python
  compiled+=("python")
else
  skipped+=("python (no python3)")
fi

# TypeScript: type-check the generated tree. tsc is installed from the SDK's own
# package.json when it has one; without it there is nothing to run.
if have npx --version && [ -f sdk/typescript/package.json ]; then
  (cd sdk/typescript && npm install --silent --no-audit --no-fund && npx --no-install tsc --noEmit)
  compiled+=("typescript")
else
  skipped+=("typescript (no package.json yet, or no npx)")
fi

# Java: compile the generated sources with the jars the generator's build file
# names. Requiring the build tool would mean installing maven for a check that
# javac can do, so javac is used directly.
if have javac -version && [ -d sdk/java/src/main/java ]; then
  # The native library has no third-party dependencies, which is why javac alone
  # is enough. If that ever changes this has to change with it.
  find sdk/java/src/main/java -name '*.java' > /tmp/applab-java-sources.txt
  javac -d /tmp/applab-java-classes @/tmp/applab-java-sources.txt
  compiled+=("java")
else
  skipped+=("java (no javac, or nothing generated yet)")
fi

# Rust: cargo check is the fastest way to know a crate builds. The generated
# crate's own manifest names its dependencies, so no extra setup is needed.
if have cargo --version && [ -f sdk/rust/Cargo.toml ]; then
  (cd sdk/rust && cargo check --quiet)
  compiled+=("rust")
else
  skipped+=("rust (no cargo, or nothing generated yet)")
fi

echo "compiled: ${compiled[*]:-none}"
if [ "${#skipped[@]}" -gt 0 ]; then
  printf 'skipped: %s\n' "${skipped[*]}"
fi

# In CI every toolchain is present, so a skip there means this script has drifted
# from the workflow rather than that a tool is genuinely absent.
if [ "${APPLAB_SDK_REQUIRE_ALL:-}" = "1" ] && [ "${#skipped[@]}" -gt 0 ]; then
  echo "APPLAB_SDK_REQUIRE_ALL=1 but some languages could not be checked" >&2
  exit 1
fi
