#!/bin/bash
# Builds openconnect.exe from OpenConnect's release tarball and bundles it
# with exactly the DLLs it loads, for the Windows installer. It runs on the
# Windows release runner in MSYS2's UCRT64 shell, which GitHub's image has
# at C:\msys64 (.github/workflows/release.yml); this is the Windows
# counterpart of build/darwin/bundle-openconnect.sh.
#
# Usage: bundle-openconnect.sh <dest> <notices>
#        bundle-openconnect.sh --check <dest>
#
# Every download is listed in openconnect-packages.txt with its SHA-256 and
# checked against it before use: the OpenConnect tarball and the MSYS2
# compiler and libraries (openconnect-lock.py writes those lines). No
# maintainer has to make or host an archive, and a release builds from what
# the repository names.
#
# <dest> (bin/openconnect-windows) gets openconnect.exe and every DLL its
# import table names, followed through each DLL's own imports; files already
# there, such as wintun.dll, stay. <notices> gets versions.txt, naming the
# MSYS2 package and version each DLL came from, and the licence texts of
# each, which the installer puts in $INSTDIR\OpenConnect for the source
# offer in NOTICE.
#
# --check fails unless every DLL in <dest> matches a line of
# openconnect-dlls.txt, whose names NOTICE must list (a Go test in
# internal/vpn/openconnect checks that). Bundling runs it last; release.yml
# also runs it on a maintainer's own archive.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)

check() {
	local dest=$1 bad= f dll pattern name covered
	for f in "$dest"/*; do
		dll=$(basename "$f" | tr 'A-Z' 'a-z')
		case $dll in *.dll) ;; *) continue ;; esac
		covered=
		while read -r pattern name; do
			case $pattern in '' | '#'*) continue ;; esac
			# shellcheck disable=SC2254 # the pattern is meant to glob
			case $dll in $pattern) covered=1 && break ;; esac
		done <"$here/openconnect-dlls.txt"
		[ -n "$covered" ] || bad="$bad $dll"
	done
	if [ -n "$bad" ]; then
		echo "NOTICE does not cover these DLLs in $dest:$bad. List each library in NOTICE and in build/windows/openconnect-dlls.txt." >&2
		return 1
	fi
}

if [ "${1:-}" = --check ]; then
	check "${2:?usage: bundle-openconnect.sh --check <dest>}"
	exit
fi
dest=${1:?usage: bundle-openconnect.sh <dest> <notices>}
notices=${2:?usage: bundle-openconnect.sh <dest> <notices>}
if [ "${MSYSTEM:-}" != UCRT64 ]; then
	echo "bundle-openconnect.sh runs in MSYS2's UCRT64 shell (MSYSTEM=UCRT64), not ${MSYSTEM:-a plain shell}" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shortcut: MSYS2 removes old package builds from repo.msys2.org after a
# while, and a pinned one that is gone fails the download here. Rerun
# openconnect-lock.py and commit the new pins when that happens.
pkgs=() tarball= tarball_url=
while read -r url sum; do
	case $url in '' | '#'*) continue ;; esac
	f="$work/$(basename "$url")"
	curl -fsSL --retry 3 -o "$f" "$url"
	echo "$sum  $f" | sha256sum -c --quiet -
	case $f in
	*.pkg.tar.zst) pkgs+=("$f") ;;
	*/openconnect-*.tar.gz) tarball=$f tarball_url=$url ;;
	*) echo "openconnect-packages.txt lists $url, which is neither a package nor the OpenConnect tarball" >&2 && exit 1 ;;
	esac
done <"$here/openconnect-packages.txt"
[ -n "$tarball" ] || { echo "openconnect-packages.txt names no OpenConnect tarball" >&2 && exit 1; }

# Only UCRT64 packages, which nothing on the runner has installed, and MSYS
# make, whose own dependencies the base install has.
pacman -U --noconfirm --noprogressbar "${pkgs[@]}"

version=$(basename "$tarball" .tar.gz)
version=${version#openconnect-}
tar -xzf "$tarball" -C "$work"
src="$work/openconnect-$version"
# Only what Hostbay uses: hostbay-netd always passes --script, so the
# default vpnc-script is never run, and no optional library is linked
# beyond GnuTLS, libxml2, zlib and libiconv.
(
	cd "$src"
	./configure --disable-shared --disable-nls --disable-docs \
		--with-vpnc-script=vpnc-script-win.js --with-builtin-json \
		--without-libproxy --without-stoken --without-libpskc \
		--without-libpcsclite --without-gssapi --without-lz4
	# Never "make" or "make all": those also download the latest
	# vpnc-script-win.js and wintun.dll, which no pin covers.
	make -j"$(nproc)" openconnect.exe
)
if [ -e "$src/vpnc-script-win.js" ] || [ -e "$src/.libs/wintun.dll" ]; then
	echo "the OpenConnect build downloaded files no pin covers" >&2
	exit 1
fi
exe="$src/.libs/openconnect.exe"
[ -f "$exe" ] || exe="$src/openconnect.exe"

mkdir -p "$dest"
strip -o "$dest/openconnect.exe" "$exe"

# The DLLs openconnect.exe imports, and theirs in turn, from UCRT64's bin.
# Windows' own (System32, and the api-ms-win-* API sets) are not bundled; a
# DLL found in neither place fails the build.
system=$(cygpath -u "$SYSTEMROOT")/System32
declare -A seen=()
queue=("$dest/openconnect.exe") dlls=()
i=0
while [ "$i" -lt "${#queue[@]}" ]; do
	for dll in $(objdump -p "${queue[$i]}" | sed -n 's/^[[:space:]]*DLL Name: //p' | tr -d '\r'); do
		key=$(echo "$dll" | tr 'A-Z' 'a-z')
		[ -z "${seen[$key]:-}" ] || continue
		seen[$key]=1
		if [ -f "/ucrt64/bin/$dll" ]; then
			cp "/ucrt64/bin/$dll" "$dest/$dll"
			queue+=("$dest/$dll")
			dlls+=("$dll")
			echo "bundled /ucrt64/bin/$dll"
		elif [[ $key == api-ms-win-* ]] || [ -f "$system/$dll" ]; then
			continue
		else
			echo "cannot find $dll, imported by $(basename "${queue[$i]}")" >&2
			exit 1
		fi
	done
	i=$((i + 1))
done

# GnuTLS reads the Windows certificate store only when it was built with no
# default trust file (lib/system/certs.c); one of the paths its configure
# looks for would point at a file no Windows machine has.
for f in /etc/ssl/ca-bundle.pem /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/cert.pem /usr/local/share/certs/ca-root-nss.crt /etc/ssl/cert.pem; do
	if grep -a -q -F "$f" "$dest"/libgnutls-*.dll; then
		echo "the bundled GnuTLS trusts $f instead of the Windows certificate store" >&2
		exit 1
	fi
done

# The bundle must run without UCRT64's bin on PATH, as on a user's machine.
out=$(PATH=/usr/bin "$dest/openconnect.exe" --version 2>&1 || true)
if ! grep -q "version v$version" <<<"$out"; then
	echo "the bundled openconnect.exe does not run on its own:" >&2
	echo "$out" >&2
	exit 1
fi

# The source offer in NOTICE names exact versions: keep each bundled file's
# package and version, and its licence texts, beside the app, since build
# logs expire.
rm -rf "$notices"
mkdir -p "$notices/openconnect-$version"
cp "$src/COPYING.LGPL" "$notices/openconnect-$version/"
echo "openconnect.exe openconnect $version $tarball_url" >"$notices/versions.txt"
for dll in "${dlls[@]}"; do
	pkg=$(pacman -Qqo "/ucrt64/bin/$dll")
	read -r _ pkgver < <(pacman -Q "$pkg")
	echo "$dll $pkg $pkgver" >>"$notices/versions.txt"
	dir="$notices/${pkg#mingw-w64-ucrt-x86_64-}-$pkgver"
	[ ! -d "$dir" ] || continue
	mkdir -p "$dir"
	pacman -Qlq "$pkg" | { grep '/share/licenses/.*[^/]$' || true; } | while read -r f; do cp "$f" "$dir/"; done
	if [ -z "$(ls -A "$dir")" ]; then
		echo "$pkg installs no licence text to ship with $dll" >&2
		exit 1
	fi
done
cat "$notices/versions.txt"

check "$dest"
echo "bundled openconnect $version into $dest"
