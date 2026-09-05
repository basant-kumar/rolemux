# Release guide

RoleMux uses GitHub Actions and GoReleaser to publish signed, notarized macOS
ARM64 and Intel archives, checksums, SBOMs, GitHub attestations, and the Homebrew
Cask.

## Pipelines

- `.github/workflows/ci.yml` validates pushes and pull requests on macOS and
  Linux. It runs formatting, vet, tests, race tests, actionlint, and a
  non-publishing GoReleaser snapshot.
- `.github/workflows/release.yml` runs only for `v*` tags. It refuses to publish
  if an Apple signing or notarization secret is absent.

For Jenkins users, the workflow YAML serves the same role as a repository
`Jenkinsfile`, while GitHub-hosted runners replace build agents and Actions
secrets replace Jenkins Credentials.

## Required secrets

- `HOMEBREW_TAP_GITHUB_TOKEN`: content-write access to the tap repository
- `MACOS_SIGN_P12`: base64-encoded Developer ID Application leaf certificate
  and private key in a password-protected `.p12`
- `MACOS_SIGN_PASSWORD`: password for that `.p12`
- `MACOS_NOTARY_KEY`: base64-encoded App Store Connect team API `.p8` key
- `MACOS_NOTARY_KEY_ID`: API key ID
- `MACOS_NOTARY_ISSUER_ID`: API issuer UUID

Keep only the leaf certificate in the `.p12`; GoReleaser resolves the Apple
chain. The workflow checks all secrets before starting a release.

## Publish

Validate locally:

```bash
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...

HOMEBREW_TAP_OWNER=basant-kumar \
HOMEBREW_TAP_REPOSITORY=homebrew-tap \
ROLEMUX_REPOSITORY_NAME=rolemux \
  goreleaser release --snapshot --clean --skip=publish
```

Commit and push the intended changes, then create exactly one release trigger:

```bash
ROLEMUX_VERSION=vX.Y.Z

git tag -a "${ROLEMUX_VERSION}" -m "RoleMux ${ROLEMUX_VERSION}"
git push origin "${ROLEMUX_VERSION}"
```

Do not separately create the GitHub release. GoReleaser owns the GitHub release
and Homebrew Cask update.

## Verify

```bash
ROLEMUX_REPOSITORY=basant-kumar/rolemux
ROLEMUX_VERSION=vX.Y.Z

gh run list --repo "${ROLEMUX_REPOSITORY}" --workflow Release --limit 1
gh release view "${ROLEMUX_VERSION}" --repo "${ROLEMUX_REPOSITORY}"
gh attestation verify <archive.tar.gz> --repo "${ROLEMUX_REPOSITORY}"
```

On macOS, extract an archive and verify its Developer ID signature:

```bash
codesign --verify --strict --verbose=4 ./rolemux
codesign -dvv ./rolemux
```

Finally update the tap and exercise the public installation path:

```bash
brew update
brew upgrade --cask rolemux
rolemux version
```
