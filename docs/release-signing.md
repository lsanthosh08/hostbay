# Release signing

Hostbay updates itself from GitHub Releases (R55). It installs an update only
when the release's `hostbay-manifest.json` carries a valid ed25519 signature
from a public key built into the running Hostbay (KTD21). The manifest lists
each release file's version, OS, architecture, name, size and SHA-256, so the
signature covers every byte Hostbay installs.

Create the key before the first Release 2 tag. Until then every build has an
empty key list, reports "updates are not signed yet" and never downloads an
update. A build with an empty list can never update itself; its users must
install the next version by hand. Without the key the release workflow's
`sign` job warns and publishes the release with no update manifest.

## How a release is signed

`.github/workflows/release.yml` builds every installer, then the `sign` job
runs `cmd/sign-manifest` over them in the `release-signing` environment:

```sh
HOSTBAY_SIGNING_KEY=... go run ./cmd/sign-manifest -version 1.2.0 -dir release
```

It writes `hostbay-manifest.json` and `hostbay-manifest.json.sig` (the base64
signature over the manifest's exact bytes), and the `release` job publishes
both with the installers. The key is read only from the environment variable
and is never printed.

## 1. Generate the key

On a computer you trust, with this repository checked out:

```sh
go run ./cmd/sign-manifest -keygen ~/hostbay-release.key
```

This writes the private key (a base64 ed25519 seed) to the file with mode 600
and prints the public key. It refuses to overwrite an existing file.

Keep an offline backup of the private key file, for example in a password
manager. Anyone holding it can push code to every Hostbay installation.

## 2. Store the private key in the protected environment

In the GitHub repository, open **Settings → Environments → New environment**
and name it `release-signing`. Then:

1. Under **Deployment protection rules**, turn on **Required reviewers** and
   add the people who approve releases. Each signing run now waits for one
   of them to approve it.
2. Under **Deployment branches and tags**, choose **Selected branches and
   tags** and add the tag rule `v*`.
3. Under **Environment secrets**, add `HOSTBAY_SIGNING_KEY` with the contents
   of the private key file.

Never store the key as a repository or organization secret: any workflow could
read it without an approval. If the environment does not exist when the
workflow runs, GitHub creates it without protection rules, and the release
publishes unsigned because the secret is missing.

Delete the working copy of the key file once the secret and the backup are
saved.

## 3. Embed the public key

Add the printed public key to `releaseKeys` in `internal/update/keys.go`:

```go
var releaseKeys = []string{
	"BASE64-PUBLIC-KEY=",
}
```

Commit it. Every build from that commit on verifies updates with this key.
`TestEmbeddedKeysParse` fails if an entry is not a valid public key.

Public keys are not secret. They live in source, not in a build flag, so a
local build and a release build trust the same keys, and changing them is a
reviewed commit.

## 4. Rotate the key

Hostbay accepts a manifest signed by any key in its list, so a key can change
without stranding anyone:

1. Generate a new key (step 1).
2. Append its public key to `releaseKeys`, keeping the old one, and ship a
   release. That release is still signed with the old key, so every
   installation can update to it.
3. Once most users run that release, replace `HOSTBAY_SIGNING_KEY` in the
   `release-signing` environment with the new private key. Later releases are
   signed with it.
4. In a later release, remove the old public key from `releaseKeys`.

If a key leaks, do steps 1–3 at once and remove the leaked key in the same
release as step 2. Installations that have not updated yet still trust the
leaked key until they install a release without it, so announce the leak on
the release page and ask users to update.

## Check a release by hand

Download `hostbay-manifest.json`, `hostbay-manifest.json.sig` and an installer
from the release, then compare the installer's SHA-256 (`shasum -a 256` or
`sha256sum`) and size with its entry in the manifest. The signature is checked
by Hostbay itself against the keys in `internal/update/keys.go`.
