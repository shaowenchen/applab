#!/usr/bin/env bash
#
# Publish each installation's address, key and usage to the job summary.
#
# The summary is where the deliverable lives: the run log scrolls away, and the
# Summary is what someone returns to when they need a link again. It is written
# by environment.sh rather than by the workflow so the addresses and the keys come
# from the process that actually knows them, not from a second guess.
#
# Two installations, so this prints two blocks. They are kept apart rather than
# merged into one table because the thing a reader must not do is pair
# installation 1's address with installation 2's key: each key opens exactly one
# of them, and a table whose rows are address/key/namespace invites exactly that
# mistake. Same layout as the closing banner in environment.sh, deliberately.
set -euo pipefail

# One URL per line, in installation order — the order environment.sh wrote them
# in. Read positionally because that is the contract: the file exists so that
# something other than this script can find out where the environment ended up,
# and line 1 meaning installation 1 is the whole of the format.
urls="${APPLAB_PUBLIC_URL_SHOWN:-}"
url_1="$(printf '%s\n' "$urls" | sed -n '1p')"
url_2="$(printf '%s\n' "$urls" | sed -n '2p')"

key_1="${APPLAB_API_KEY_SHOWN:-}"
key_2="${APPLAB_API_KEY_2_SHOWN:-}"
# The public URLs already carry the installation's base path — they are the
# console's addresses — so the app prefix is appended to them, not to the host.
prefix="${APPLAB_PATH_PREFIX_SHOWN:-/apps}"

{
  echo "## AppLab is ready"
  echo
  echo "Two installations on one cluster. They share the cluster and the object"
  echo "store; they share nothing else. **An app created in one does not appear"
  echo "in the other.**"
  echo

  # Printed per installation rather than as one table with two rows, so that the
  # address, the key and the namespace a reader is about to use are in one block
  # and cannot be read across.
  for n in 1 2; do
    case "$n" in
      1) url="$url_1"; key="$key_1" ;;
      2) url="$url_2"; key="$key_2" ;;
    esac
    echo "### Installation ${n}"
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
    echo "| Namespace | \`applab-${n}\` |"
    echo
  done

  echo "| | |"
  echo "|---|---|"
  if [ -n "${APPLAB_VERSION_SHOWN:-}" ]; then
    echo "| AppLab | \`${APPLAB_VERSION_SHOWN}\` |"
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
  echo "export APPLAB_URL='${url_1}'"
  echo "export APPLAB_KEY='${key_1}'"
  echo
  echo "# From any directory with a Dockerfile:"
  echo "applab push myshop"
  echo '```'
  echo
  echo "The app is then served at \`${url_1}${prefix}/myshop/\`, and appears in"
  echo "installation 1's console. Pointing \`APPLAB_URL\` at \`${url_2}\` with"
  echo "\`APPLAB_KEY='${key_2}'\` does the same against installation 2, as a second"
  echo "and independent app of the same name."
  echo
  echo "The environment is discarded when this run ends — cancel the workflow to end it early."
} >> "${GITHUB_STEP_SUMMARY:-/dev/stdout}"

# A notice per installation, in the run's timeline, so a link is visible without
# opening the summary — the same courtesy the tunnel agents' own output would have
# given.
#
# The keys are deliberately not passed through ::add-mask::. Each is the
# deliverable, and a masked value is redacted wherever it appears — including in
# the summary above, which exists to show it. Masking them would leave this
# environment's whole output a pair of asterisks. That is also why environment.sh
# keeps each key in a variable and never builds a command line out of one: what
# keeps a key out of the log is that the ERR trap prints the command's
# unexpanded text, not that GitHub is hiding it afterwards.
for n in 1 2; do
  case "$n" in
    1) url="$url_1"; key="$key_1" ;;
    2) url="$url_2"; key="$key_2" ;;
  esac
  if [ -n "$url" ]; then
    echo "::notice title=AppLab ${n}::${url}"
  fi
  if [ -n "$key" ]; then
    echo "::notice title=AppLab ${n} API key::${key}"
  fi
done
