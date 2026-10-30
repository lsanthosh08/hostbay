#!/bin/sh
# Bundles openconnect and the non-system libraries it links into a macOS .app.
# The binary goes in Contents/MacOS beside hostbay-vpn, the only place
# internal/vpn/openconnect looks for it on macOS, and each library goes in
# Contents/Frameworks with every install name rewritten to
# @executable_path/../Frameworks, so the bundle runs on a Mac without Homebrew.
#
# Usage: bundle-openconnect.sh <App.app/Contents> [openconnect binary]
#
# The binary defaults to the openconnect on PATH (Homebrew's in release CI).
# Without one, the app is built without it and OpenConnect profiles report
# it missing. A bundle that cannot be made self-contained fails the build:
# it would run on the build machine and break on every other Mac.
#
# Homebrew's bottle is single-architecture, so the universal app carries an
# openconnect for the build machine's architecture only.
set -eu

contents=${1:?usage: bundle-openconnect.sh <App.app/Contents> [openconnect binary]}
src=${2:-$(command -v openconnect || true)}
if [ -z "$src" ]; then
	echo "openconnect not found: building without bundled OpenConnect (brew install openconnect to bundle it)"
	exit 0
fi
if ! command -v otool >/dev/null 2>&1; then
	echo "otool not found: building without bundled OpenConnect (bundling needs macOS)"
	exit 0
fi

src=$(realpath "$src")
name=$(basename "$src")
frameworks="$contents/Frameworks"
mkdir -p "$contents/MacOS" "$frameworks"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
queue="$work/queue" # lines of "<original path> <bundled path>"

rm -f "$contents/MacOS/$name"
cp "$src" "$contents/MacOS/$name"
chmod 0755 "$contents/MacOS/$name"
printf '%s %s\n' "$src" "$contents/MacOS/$name" >"$queue"

# resolve prints the file a load command in orig refers to, or nothing for a
# system library. @loader_path and @rpath resolve against orig's real
# directory, as dyld would have resolved them there.
resolve() {
	ref=$1 orig=$2
	dir=$(dirname "$orig")
	case $ref in
	/usr/lib/* | /System/*) return 0 ;;
	@loader_path/*) set -- "$dir/${ref#@loader_path/}" ;;
	@executable_path/*) set -- "$(dirname "$src")/${ref#@executable_path/}" ;;
	@rpath/*)
		lib=${ref#@rpath/}
		set --
		for rp in $(otool -l "$orig" | awk '$1 == "path" && prev ~ /LC_RPATH/ { print $2 } { if ($1 == "cmd") prev = $2 }'); do
			case $rp in @loader_path/*) rp="$dir/${rp#@loader_path/}" ;; esac
			set -- "$@" "$rp/$lib"
		done
		set -- "$@" "$dir/$lib" "$dir/../lib/$lib"
		;;
	*) set -- "$ref" ;;
	esac
	for p in "$@"; do
		if [ -f "$p" ]; then
			realpath "$p"
			return 0
		fi
	done
	echo "cannot find $ref, linked by $orig" >&2
	return 1
}

i=1
while line=$(sed -n "${i}p" "$queue") && [ -n "$line" ]; do
	orig=${line%% *} copy=${line#* }
	self=$(otool -D "$orig" | sed -n 2p)
	for ref in $(otool -L "$orig" | sed 1d | awk '{ print $1 }'); do
		[ "$ref" = "$self" ] && continue
		lib=$(resolve "$ref" "$orig")
		[ -n "$lib" ] || continue
		base=$(basename "$ref")
		if ! awk -v p="$frameworks/$base" '$2 == p { found = 1 } END { exit !found }' "$queue"; then
			rm -f "$frameworks/$base"
			cp "$lib" "$frameworks/$base"
			chmod 0644 "$frameworks/$base"
			install_name_tool -id "@executable_path/../Frameworks/$base" "$frameworks/$base"
			printf '%s %s\n' "$lib" "$frameworks/$base" >>"$queue"
			echo "bundled $lib"
		fi
		install_name_tool -change "$ref" "@executable_path/../Frameworks/$base" "$copy"
	done
	i=$((i + 1))
done

# install_name_tool invalidates the signature, and arm64 kills an unsigned
# Mach-O at exec; the app's own codesign later re-signs the whole bundle.
awk '{ print $2 }' "$queue" | while read -r f; do codesign --force --sign - "$f"; done

leftover=$(awk '{ print $2 }' "$queue" | while read -r f; do otool -L "$f" | sed 1d | awk '{ print $1 }'; done |
	grep -v -e '^/usr/lib/' -e '^/System/' -e '^@executable_path/../Frameworks/' || true)
if [ -n "$leftover" ]; then
	echo "the bundled openconnect still links outside the app:" >&2
	echo "$leftover" >&2
	exit 1
fi
"$contents/MacOS/$name" --version >/dev/null 2>&1 || {
	echo "the bundled $name does not run" >&2
	exit 1
}

# The LGPL source offer in NOTICE names exact versions: keep the path of each
# file bundled (Homebrew's Cellar/<formula>/<version>) and its keg's licence
# texts in the app, since build logs expire.
notices="$contents/Resources/OpenConnect"
rm -rf "$notices"
mkdir -p "$notices"
awk '{ print $1 }' "$queue" >"$notices/versions.txt"
sed -n 's|^\(.*/Cellar/[^/]*/[^/]*\)/.*|\1|p' "$notices/versions.txt" | sort -u | while read -r keg; do
	dest="$notices/$(basename "$(dirname "$keg")")-$(basename "$keg")"
	mkdir -p "$dest"
	for f in "$keg"/COPYING* "$keg"/LICENSE*; do
		if [ -f "$f" ]; then cp "$f" "$dest/"; fi
	done
done
echo "bundled $src into $contents"
