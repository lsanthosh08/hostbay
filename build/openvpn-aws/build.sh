#!/bin/sh
# Builds openvpn-aws: upstream OpenVPN with Hostbay's AWS Client VPN patch
# (openvpn-2.7.9-aws.patch), linked statically against OpenSSL, LZO, LZ4
# and, on Linux, libcap-ng. It sits beside the stock openvpn, so it must not
# bring libraries of its own: on macOS the app's Frameworks folder holds the
# stock openvpn's dylibs by file name.
#
# Usage: build.sh [--source-archive <file.tar.gz>] <output dir>
#
# Writes <output dir>/openvpn-aws. On macOS it is universal (arm64 and
# x86_64, built separately and joined with lipo); on Linux it is for the
# build machine's architecture. --source-archive also writes a tarball of
# every verified upstream tarball, the patch and these build files, which a
# release attaches as the source of the modified OpenVPN.
#
# Needs a C compiler, make, perl (OpenSSL's Configure), curl and tar; on
# macOS the Xcode command line tools. On Linux OpenVPN's configure also
# insists on pkg-config being installed, though every library here is
# passed to it directly. Work files go in a temporary directory under
# $TMPDIR, removed on exit.
set -eu

usage="usage: build.sh [--source-archive <file.tar.gz>] <output dir>"
archive=
out=
while [ $# -gt 0 ]; do
	case $1 in
	--source-archive) archive=${2:?$usage}; shift 2 ;;
	-*) echo "$usage" >&2; exit 2 ;;
	*) [ -z "$out" ] || { echo "$usage" >&2; exit 2; }; out=$1; shift ;;
	esac
done
[ -n "$out" ] || { echo "$usage" >&2; exit 2; }

here=$(cd "$(dirname "$0")" && pwd)
. "$here/versions.env"
os=$(uname -s)
jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)
mkdir -p "$out"
out=$(cd "$out" && pwd)
case $archive in "" | /*) ;; *) archive=$(pwd)/$archive ;; esac

work=$(mktemp -d "${TMPDIR:-/tmp}/openvpn-aws.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir "$work/dl"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# Every tarball is downloaded and checked before any is unpacked, so a
# tampered or replaced upstream file never reaches tar or a compiler.
fetch() { # <url> <sha256>
	f=$work/dl/$(basename "$1")
	curl -fsSL --retry 3 -o "$f" "$1"
	got=$(sha256 "$f")
	if [ "$got" != "$2" ]; then
		echo "error: $(basename "$1") has SHA-256 $got but versions.env pins $2; not unpacking it" >&2
		exit 1
	fi
	echo "verified $(basename "$1")"
}
fetch "$OPENVPN_URL" "$OPENVPN_SHA256"
fetch "$OPENSSL_URL" "$OPENSSL_SHA256"
fetch "$LZO_URL" "$LZO_SHA256"
fetch "$LZ4_URL" "$LZ4_SHA256"
if [ "$os" = Linux ]; then fetch "$LIBCAPNG_URL" "$LIBCAPNG_SHA256"; fi

# OpenSSL compiles its prefix into libcrypto as the place to read
# openssl.cnf, providers and engines from. The helper runs openvpn-aws as
# root, so that prefix is a root-owned path no user can create; the files
# are staged under the work directory with DESTDIR instead.
ssl_prefix=/var/empty/openvpn-aws
min=

# build_arch <dir> <autoconf host, or "" for this machine> <OpenSSL target> [-arch flag]
# Builds the libraries and openvpn into <dir>; the binary is <dir>/openvpn-aws.
# OpenSSL's darwin64 targets set -arch themselves and refuse a second one.
build_arch() {
	dir=$1 host=${2:+--host=$2} ssl_target=$3
	cc="cc ${4:-} $min"
	pre=$dir/prefix
	ssl=$dir/ssl$ssl_prefix
	mkdir -p "$dir" "$pre"
	for f in "$work"/dl/*.tar.gz; do tar -xzf "$f" -C "$dir"; done

	(cd "$dir/openssl-$OPENSSL_VERSION" &&
		perl ./Configure $ssl_target $min no-shared no-module no-engine no-tests no-docs no-apps \
			--prefix=$ssl_prefix --openssldir=$ssl_prefix/ssl --libdir=lib >/dev/null &&
		make -j"$jobs" build_libs >/dev/null &&
		make install_dev DESTDIR="$dir/ssl" >/dev/null)

	(cd "$dir/lzo-$LZO_VERSION" &&
		./configure $host --prefix="$pre" --disable-shared --enable-static CC="$cc" >/dev/null &&
		make -j"$jobs" >/dev/null && make install >/dev/null)

	make -C "$dir/lz4-$LZ4_VERSION/lib" -j"$jobs" install CC="$cc" PREFIX="$pre" BUILD_SHARED=no >/dev/null

	capng=
	if [ "$os" = Linux ]; then
		(cd "$dir/libcap-ng-$LIBCAPNG_VERSION" &&
			./configure $host --prefix="$pre" --disable-shared --enable-static \
				--without-python3 CC="$cc" >/dev/null &&
			make -C src -j"$jobs" >/dev/null && make -C src install >/dev/null)
		capng="LIBCAPNG_CFLAGS=-I$pre/include LIBCAPNG_LIBS=$pre/lib/libcap-ng.a"
	fi

	# Libraries are named as .a files so nothing can resolve to a shared
	# copy. LZ4 is the exception: configure probes it with -llz4, which
	# finds the static library because its prefix holds no other. No PKCS#11
	# or plugins: they would load third-party libraries. No DCO: macOS has
	# none and on Linux it needs libnl.
	ovpn=$dir/openvpn-$OPENVPN_VERSION
	(cd "$ovpn" && patch -p1 -F0 -N <"$here/openvpn-$OPENVPN_VERSION-aws.patch")
	sys_libs=
	if [ "$os" = Linux ]; then sys_libs="-pthread -ldl"; fi
	# shellcheck disable=SC2086 # $host and $capng are empty or whole words
	(cd "$ovpn" &&
		./configure $host --disable-pkcs11 --disable-plugins \
			--disable-plugin-auth-pam --disable-plugin-down-root --disable-dco \
			--disable-unit-tests --enable-lzo --enable-lz4 CC="$cc" \
			OPENSSL_CFLAGS="-I$ssl/include" OPENSSL_LIBS="$ssl/lib/libssl.a $ssl/lib/libcrypto.a $sys_libs" \
			LZO_CFLAGS="-I$pre/include" LZO_LIBS="$pre/lib/liblzo2.a" \
			LZ4_CFLAGS="-I$pre/include" LZ4_LIBS="-L$pre/lib -llz4" \
			$capng >/dev/null &&
		make -C src/compat -j"$jobs" >/dev/null && make -C src/openvpn -j"$jobs" >/dev/null)
	cp "$ovpn/src/openvpn/openvpn" "$dir/openvpn-aws"
}

case $os in
Darwin)
	# The app's minimum macOS (LSMinimumSystemVersion in build/darwin/Info.plist).
	export MACOSX_DEPLOYMENT_TARGET=13.0
	min=-mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET
	build_arch "$work/arm64" aarch64-apple-darwin darwin64-arm64-cc "-arch arm64"
	build_arch "$work/x86_64" x86_64-apple-darwin darwin64-x86_64-cc "-arch x86_64"
	lipo -create "$work/arm64/openvpn-aws" "$work/x86_64/openvpn-aws" -output "$out/openvpn-aws"
	;;
Linux)
	build_arch "$work/native" "" linux-"$(uname -m)"
	cp "$work/native/openvpn-aws" "$out/openvpn-aws"
	;;
*)
	echo "error: build.sh builds on macOS and Linux; use build.ps1 on Windows" >&2
	exit 1
	;;
esac

# Smoke checks: the right version, and no library outside the system's.
ver=$("$out/openvpn-aws" --version 2>&1 | head -n 1)
case $ver in
"OpenVPN $OPENVPN_VERSION "*) echo "$ver" ;;
*) echo "error: openvpn-aws --version printed: $ver" >&2; exit 1 ;;
esac
dirs=$(strings -a "$out/openvpn-aws" | grep -E '^(OPENSSLDIR|MODULESDIR|ENGINESDIR): ' | sort -u)
if [ "$(echo "$dirs" | grep -c ": \"$ssl_prefix/")" -ne 3 ]; then
	echo "error: OpenSSL's built-in paths are not all under $ssl_prefix:" >&2
	echo "$dirs" >&2
	exit 1
fi
if [ "$os" = Darwin ]; then
	foreign=$(otool -L "$out/openvpn-aws" | awk 'NF > 1 && $NF !~ /:$/ && $1 !~ /^\/(usr\/lib|System)\//' || true)
else
	foreign=$(ldd "$out/openvpn-aws" | awk '$1 !~ /^(linux-vdso|\/lib.*\/ld-linux|libc\.so|libm\.so|libpthread\.so|libdl\.so|libresolv\.so|librt\.so)/' || true)
fi
if [ -n "$foreign" ]; then
	echo "error: openvpn-aws links libraries that are not the system's:" >&2
	echo "$foreign" >&2
	exit 1
fi

if [ -n "$archive" ]; then
	src=$work/openvpn-aws-$OPENVPN_VERSION-source
	mkdir -p "$src"
	cp "$work"/dl/*.tar.gz "$here/openvpn-$OPENVPN_VERSION-aws.patch" "$here/versions.env" \
		"$here/build.sh" "$here/build.ps1" "$here/README.md" "$src/"
	COPYFILE_DISABLE=1 tar -czf "$archive" -C "$work" "$(basename "$src")"
	echo "wrote $archive"
fi
echo "wrote $out/openvpn-aws"
