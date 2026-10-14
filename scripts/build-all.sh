#!/bin/bash
# Cross-compile the whole matrix and print a size report. Convenience wrapper
# around build.sh for release checks.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec "$HERE/scripts/build.sh" --all "$@"
