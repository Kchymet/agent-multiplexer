#!/bin/sh
# Print the amux product version for the checkout this runs in.
#
#   release build (AMUX_RELEASE=1):  0.1.<commit count>
#   checkout build (default):        0.1.<commit count>-dev+<sha12>[.dirty]
#
# master is squash-merged and linear, so the first-parent commit count is a
# monotonic patch number: every push to master gets the next release version,
# and the release workflow tags it v0.1.<count>. MAJOR.MINOR is bumped here by
# hand; the patch keeps counting so versions never go backwards. The -dev
# prerelease makes a checkout build sort below the release of the same count
# and marks it as a development build that self-update never replaces on its
# own. Outside a Git checkout (a source tarball) it reports 0.0.0-dev.
set -eu
MAJOR_MINOR=0.1
cd "$(dirname "$0")/.."
if ! count=$(git rev-list --first-parent --count HEAD 2>/dev/null); then
	echo "0.0.0-dev"
	exit 0
fi
if [ "${AMUX_RELEASE:-}" = 1 ]; then
	if [ "$(git rev-parse --is-shallow-repository)" = true ]; then
		echo "version.sh: a release version needs full history (fetch-depth: 0)" >&2
		exit 1
	fi
	echo "$MAJOR_MINOR.$count"
	exit 0
fi
sha=$(git rev-parse --short=12 HEAD)
dirty=
if [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then
	dirty=.dirty
fi
echo "$MAJOR_MINOR.$count-dev+$sha$dirty"
