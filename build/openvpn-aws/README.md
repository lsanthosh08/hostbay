# openvpn-aws

`openvpn-aws` is upstream OpenVPN 2.7.9 with a small patch that lets it sign
in to AWS Client VPN endpoints that use SAML (federated) authentication.
Hostbay ships it beside the stock `openvpn` and runs it only for AWS SSO
profiles. Every other OpenVPN profile keeps using the stock binary.

It is linked statically against OpenSSL, LZO and LZ4, and against libcap-ng
on Linux, so it brings no libraries of its own. On macOS that matters
because the app's `Frameworks` folder already holds the stock openvpn's
dylibs, matched by file name.

## Files

| File | Purpose |
|---|---|
| `openvpn-2.7.9-aws.patch` | Hostbay's changes to the OpenVPN 2.7.9 release tarball |
| `versions.env` | Pinned versions, URLs and SHA-256s of every source tarball, plus the vcpkg commit for Windows |
| `build.sh` | macOS and Linux build |
| `build.ps1` | Windows build (MSVC, CMake, vcpkg); also outputs `tapctl.exe` |

## Building

macOS or Linux:

    build/openvpn-aws/build.sh [--source-archive <file.tar.gz>] <output dir>

This needs a C compiler, make, perl, curl and tar. On macOS the Xcode
command line tools provide them, and Homebrew is not needed. On Linux,
OpenVPN's configure also insists that pkg-config is installed, although the
script passes every library to it directly (on Debian and Ubuntu:
`build-essential perl curl ca-certificates pkg-config`). The script:

1. Downloads every tarball listed in `versions.env` into a temporary
   directory and checks each one's SHA-256. A mismatch stops the build before
   anything is unpacked.
2. Builds OpenSSL (`no-shared no-module no-engine`), LZO and LZ4 as static
   libraries, plus libcap-ng on Linux.
3. Applies the patch with `patch -p1 -F0`, which allows no fuzz. If an
   upstream release moves the patched code, the build stops instead of
   patching the wrong place.
4. Configures OpenVPN without PKCS#11, plugins or DCO, and builds it.
5. On macOS, builds arm64 and x86_64 separately with a minimum of macOS 13.0
   (the app's `LSMinimumSystemVersion`) and joins them with `lipo -create`.
   On Linux, it builds for the build machine's architecture.
6. Checks the result. `--version` must report the pinned version, and the
   binary may link only system libraries: `/usr/lib` and `/System` on macOS
   (from `otool -L`), and the glibc libraries on Linux (from `ldd`).

A macOS build takes about two minutes on an Apple-silicon Mac. A Linux build
takes about 75 seconds.

`--source-archive` also writes a tarball holding the verified upstream
tarballs, the patch and these build files. A release attaches it as the
source of the modified OpenVPN.

Windows, from a Developer PowerShell for Visual Studio 2022 (x64) with CMake
3.21 or newer, Git and Python 3 on PATH:

    build\openvpn-aws\build.ps1 [-SourceArchive <file.zip>] <output dir>

It verifies and patches the OpenVPN tarball the same way, using `git apply`,
which also refuses a hunk that does not match exactly. It then builds
`openvpn-aws.exe` and `tapctl.exe` with upstream's CMake build using the
`x64-windows-static` triplet and the static C runtime. On Windows, OpenSSL,
LZO and LZ4 come from vcpkg at the commit `versions.env` pins, so their
versions are vcpkg's (OpenSSL 3.6.5, LZ4 1.10.0, LZO 2.10), not the macOS and
Linux pins: vcpkg's version database has no OpenSSL 3.5.9 or LZ4 1.11.1.
The Windows source archive includes the library tarballs vcpkg downloaded
and the vcpkg ports it built them with.

### Choices worth knowing

- **OpenSSL's built-in paths are root-owned.** OpenSSL compiles its install
  prefix into libcrypto as the place to read `openssl.cnf`, providers and
  engines from. The helper runs `openvpn-aws` as root. With a prefix under a
  temporary directory, any local user could plant a configuration there that
  root would load. The prefix is therefore `/var/empty/openvpn-aws`, a path
  only root can create, and the files are staged with `DESTDIR`. On Windows,
  vcpkg leaves OpenSSL's defaults under `C:\Program Files`. Both scripts
  check these paths in the finished binary and fail if one is elsewhere.
- **No DCO on macOS and Linux.** macOS has no OpenVPN DCO. On Linux, DCO
  needs libnl, which would be a dynamic third-party library. OpenVPN's
  Windows build cannot turn DCO off, and Hostbay's helper uses the
  ovpn-dco-win adapter that `tapctl.exe` creates.
- **libcap-ng 0.8.5 on Linux.** It is the newest release upstream publishes
  as a tarball with a generated `configure`. The 0.9 releases exist only as
  git tags, which would need autoconf, automake, libtool and llvm-strip.
- **An 8 MB main-thread stack on Windows.** The patch makes `struct
  user_pass` 256 KB, and OpenVPN keeps several on the stack. MSVC's default
  stack is 1 MB; macOS and Linux already give the main thread 8 MB.
- **Checksums.** Each SHA-256 in `versions.env` matches the one its upstream
  publishes: the GitHub release digest for OpenVPN, OpenSSL and LZ4, and the
  `.sha256` files for OpenSSL and LZ4. LZO and libcap-ng publish none, so
  their hashes were checked against Debian's `lzo2_2.10.orig.tar.gz` and
  Fedora's `libcap-ng-0.8.5.tar.gz`.

## Why each patch hunk exists

AWS Client VPN's SAML flow sends the whole SAML response from the identity
provider as the password, in the form `CRV1::<session id>::<SAML response>`.
That response often runs to tens of kilobytes. Stock OpenVPN cannot carry
it, and AWS's server expects the longer field encoding that AWS's own client
uses. Every working AWS patch (for 2.4.9, 2.5.1 and 2.6.12) changes the same
places:

- **`src/openvpn/misc.h`: `USER_PASS_LEN` becomes `(1 << 17)` (128 KB)** in
  both the `ENABLE_PKCS11` and the plain branch, so the buffer can hold the
  response whichever way OpenVPN is configured. Upstream allows 128 bytes, or
  4096 with PKCS#11. The value is parenthesised: AWS's patches write a bare
  `1 << 17`, which turns OpenVPN's `USER_PASS_LEN - 1` into `1 << 16`.
- **`src/openvpn/common.h`: `TLS_CHANNEL_BUF_SIZE` becomes `(1 << 18)`
  (256 KB).** The username and password travel in the key method 2 message
  on the TLS control channel, and upstream's 2048-byte buffer cannot hold
  them. `BUF_SIZE_MAX` stays at upstream's 1,000,000, which is still above
  the largest buffer the patch creates. AWS's patches also raise it, but
  that change is not needed here.
- **`src/openvpn/ssl.c`: strings in the key method 2 message get a 32-bit
  length.** `write_string` and `write_empty_string` write a u32 length
  instead of a u16. 2.7.9's `write_string` also caps strings at 65535 bytes,
  a cap 2.6 did not have, so the patch lifts it; `buf_write` still refuses
  anything that does not fit the buffer. `read_string` keeps the u16 length
  the server sends.
- **`src/openvpn/ssl.c`: the message length goes in its first four bytes.**
  `key_method_2_write` overwrites the leading zero uint32 with the length of
  the whole message, in host byte order, exactly as AWS's client does. AWS's
  server may check for it. Do not "fix" the byte order: the client this
  copies runs on little-endian machines, as Hostbay's targets all do.

Because of the 32-bit lengths, `openvpn-aws` cannot connect to a stock
OpenVPN server, which reads 16-bit ones. That is why it is a second binary
rather than a replacement for `openvpn`.

The AWS patches also enlarge the option-line, management-interface and
error buffers. Those changes let a password be fed in as one long
management line. Hostbay does not need them, because it sends the password
through OpenVPN 2.7's multi-line base64 management input instead, and that
path is already sized from `USER_PASS_LEN`.

## Moving to a new OpenVPN release

1. Update `OPENVPN_VERSION`, `OPENVPN_URL` and `OPENVPN_SHA256` in
   `versions.env`. Take the SHA-256 from the release's GitHub digest or check
   its GPG signature; never compute it from your own download alone.
2. Rebase the patch against two clean copies of the new tarball:

       mkdir rebase && cd rebase
       tar xzf openvpn-X.Y.Z.tar.gz && mv openvpn-X.Y.Z a
       tar xzf openvpn-X.Y.Z.tar.gz && mv openvpn-X.Y.Z b
       (cd b && patch -p1 < ../../openvpn-2.7.9-aws.patch)  # fuzz allowed here
       # redo any rejected hunk by hand, check every hunk above still
       # makes sense in b/, then:
       diff -ruN a b | sed -E "s/^((---|\+\+\+) [^$(printf '\t')]+)$(printf '\t').*/\1/" > openvpn-X.Y.Z-aws.patch

   Read the new `write_string`, `key_method_2_write` and every use of
   `USER_PASS_LEN` and `TLS_CHANNEL_BUF_SIZE`. A new cap or a new
   stack-allocated `struct user_pass` is easy to miss.
3. Rename the patch to `openvpn-X.Y.Z-aws.patch`. Both scripts find it by
   version. Then delete the old one.
4. Unpack a third clean copy and check that `patch -p1 -F0 --dry-run`
   applies the new patch. Then run `build.sh` on macOS and Linux, and
   `build.ps1` on Windows. Each one checks the version and the linked
   libraries itself.

Moving a library to a new version only needs its three lines in
`versions.env` updated, with a checksum taken from upstream.
