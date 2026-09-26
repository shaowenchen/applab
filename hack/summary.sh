#!/usr/bin/env bash
#
# Publish the environment's address, key and usage to the job summary.
#
# The summary is where the deliverable lives: the run log scrolls away, and the
# Summary is what someone returns to when they need the link again. It is
# written by environment.sh rather than by the workflow so the address and the
# key come from the process that actually knows them, not from a second guess.
set -euo pipefail

url="${APPLAB_PUBLIC_URL:-}"
key="${APPLAB_API_KEY_SHOWN:-}"
# The public URL already carries the installation's base path — it is the
# console's address — so the app prefix is appended to it, not to the host.
prefix="${APPLAB_PATH_PREFIX_SHOWN:-/apps}"

{
  echo "## AppLab is ready"
  echo
  if [ -n "$url" ]; then
    echo "**Open the console:** <${url}>"
    echo
    echo "Sign in with that address and the key below. Both are kept in your browser."
  else
    echo "**No public URL was published.** The tunnel did not report a hostname;"
    echo "check the tunnel output in the log above."
  fi
  echo
  echo "| | |"
  echo "|---|---|"
  if [ -n "$key" ]; then
    echo "| API key | \`${key}\` |"
  fi
  if [ -n "$url" ]; then
    echo "| Console | <${url}> |"
    echo "| Apps | \`${url}${prefix}/<app>/\` |"
  fi
  if [ -n "${APPLAB_VERSION_SHOWN:-}" ]; then
    echo "| AppLab | \`${APPLAB_VERSION_SHOWN}\` |"
  fi
  if [ -n "${APPLAB_NAMESPACE_SHOWN:-}" ]; then
    echo "| Namespace | \`${APPLAB_NAMESPACE_SHOWN}\` |"
  fi
  if [ -n "${APPLAB_CLUSTER_SHOWN:-}" ]; then
    echo "| Cluster | kind \`${APPLAB_CLUSTER_SHOWN}\` |"
  fi
  if [ -n "${APPLAB_REGISTRY_SHOWN:-}" ]; then
    echo "| Registry | \`${APPLAB_REGISTRY_SHOWN}\` |"
  fi
  echo
  echo "### Deploy an app"
  echo
  echo '```bash'
  echo "export APPLAB_URL='${url}'"
  echo "export APPLAB_KEY='${key}'"
  echo
  echo "# From any directory with a Dockerfile:"
  echo "applab push myshop"
  echo '```'
  echo
  echo "The app is then served at \`${url}${prefix}/myshop/\`, and appears in the console."
  echo
  echo "The environment is discarded when this run ends — cancel the workflow to end it early."
} >> "${GITHUB_STEP_SUMMARY:-/dev/stdout}"

# A notice in the run's timeline, so the link is visible without opening the
# summary — the same courtesy the tunnel agents' own output would have given.
if [ -n "$url" ]; then
  echo "::notice title=AppLab is ready::${url}"
fi
if [ -n "$key" ]; then
  # Deliberately NOT ::add-mask::. The key is the deliverable, and masking it
  # would hide it from the very summary that exists to show it.
  echo "::notice title=AppLab API key::${key}"
fi
