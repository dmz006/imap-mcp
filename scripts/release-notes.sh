#!/bin/sh
# release-notes.sh VERSION — print that version's section of CHANGELOG.md
# (without its heading). VERSION may carry a leading "v". Exits 1 when the
# changelog has no section for it.
set -eu
v=${1#v}
notes=$(awk -v v="$v" '
	/^## \[/ { if (found) exit; if (index($0, "## [" v "]") == 1) { found = 1; next } }
	found { print }
	END { if (!found) exit 1 }
' "$(dirname "$0")/../CHANGELOG.md") || { echo "no CHANGELOG section for $v" >&2; exit 1; }
printf '%s\n' "$notes" | sed -e '/./,$!d'
