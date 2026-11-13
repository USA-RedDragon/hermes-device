# Releases and verification

From v<!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const --> on, every release has GitHub artifact attestations, and `talaria update` installs a release only when the attestation of its archive verifies. This page says what is attested, what `update` checks, what it trusts, and how to check a file by hand.

## 1. What the release workflow attests

`.github/workflows/release.yaml` runs on a push of a `v*` tag. After the shared GoReleaser workflow has built and uploaded the release, and the `firefox-extension` job has uploaded the signed `.xpi`:

0. Before the build, `scripts/check-plugin-version.sh` fails the release when `PLUGIN_VERSION` in `plugin/device/core.py` or the version in `plugin/device/plugin.yaml` is not the tag's version, so the plugin in the image and in the binary reports the release it ships in. Commit `plugin: report version X.Y.Z` before tagging `vX.Y.Z`. Pre-release tags are not checked.
1. The `attest-files` job downloads the release's `.tar.gz` archives, its checksum file and the `.xpi`, checks the archives against the checksum file, and attests all of them with `actions/attest-build-provenance`. That is one SLSA build provenance v1 statement with one subject per file.
2. The `attest-images` job finds the digest of the image index of `ghcr.io/usa-reddragon/talaria:<tag>` and `ghcr.io/usa-reddragon/talaria-sandbox:<tag>` (`hermes-device` and `hermes-device-sandbox` up to v0.25.4) and attests each digest. It also pushes the attestation to GHCR next to the image.

The attestation is signed with a short-lived certificate from the Sigstore public-good Fulcio. The certificate names the workflow run: the workflow file and ref, the repository and its numeric ID, the owner and its numeric ID, the commit, the event and the runner type. The signature is recorded in the Rekor transparency log. GitHub stores the attestation; anyone can read it with `GET /repos/USA-RedDragon/talaria/attestations/sha256:<digest>`.

The jobs attest the files as the release holds them after the upload, and the image that the tag names at that time. Someone who can write to the repository's releases or packages in the minutes between the upload and the attestation could swap a file first. Both jobs refuse a release that was published more than 3 hours before, so a re-run of the job cannot attest a file that was changed later in an old release. Inside that window the attestation does not protect against a repository write token: such a token can also push a tag and run the release workflow. It protects against a release file or checksum file changed after that window, a changed download on the way, and a mirror.

## 2. What `update` checks

`update` does these steps in order and stops at the first that fails. It installs nothing when one fails.

1. It downloads the checksum file and the archive for this OS and architecture, and checks the archive's SHA-256 against the checksum file. The archive stays in memory from here on.
2. It gets the attestations for that SHA-256 from the GitHub API (only `https://slsa.dev/provenance/v1` statements, up to 5 pages of 30). No attestation at all fails.
3. It uses the trusted root of the Sigstore public-good instance that is built into the binary: `internal/attest/trusted_root.json`, the `targets/trusted_root.json` of [sigstore/root-signing](https://github.com/sigstore/root-signing) as of <!-- docgen:const pkg=internal/attest name=PublicGoodRootDate code=false -->2026-11-11<!-- /docgen:const -->. It does not download the trusted root.
4. For each attestation, it verifies the Sigstore bundle with its own verifier, which uses only the Go standard library. It checks the same things that `gh attestation verify` checks for a public-good bundle:
   - The bundle is a v0.1, v0.2 or v0.3 Sigstore bundle with a DSSE envelope of an in-toto statement and a signing certificate (a v0.3 bundle must hold the certificate alone, not a chain).
   - The transparency log entry is from the Rekor log of the trusted root. Its signed entry timestamp verifies with the Rekor key over the entry body, integrated time, log ID and log index. The entry body (kind `dsse` 0.0.1 or `intoto` 0.0.2) records the SHA-256 of this envelope's payload, this envelope's signature and this certificate. The inclusion proof (required from bundle v0.2 on) leads to its root hash, and its checkpoint is signed by the Rekor key and names the same tree size and root hash.
   - The certificate was valid at the entry's integrated time, chains to a Fulcio root and intermediate of the trusted root that were valid then, and allows code signing.
   - At least one signed certificate timestamp in the certificate verifies with a certificate transparency log key of the trusted root.
   - The DSSE signature verifies with the certificate's key over the DSSE pre-authentication encoding.
   - The statement is an in-toto v1 statement with a subject with the archive's SHA-256.

   The verifier refuses a JSON object with two equal keys or two keys that differ only in case, and a certificate extension that is not a DER UTF8String. When a bundle is signed by a certificate authority or logged in a log that the built-in trusted root does not have, `update` says that the binary's trusted root may be out of date. Then verify the release by hand (section 5) and install it by hand, or install a newer release the same way, which has a newer trusted root.
5. It checks the certificate against this policy. Every item must match exactly. The repository was renamed from `hermes-device` to `talaria` after release 0.19.1, so the repository name in the certificate may be either; the repository ID pins the repository, and the subject, build config URI and source repository must all name the same one of the two:

| Field | Required value |
| --- | --- |
| Subject (SAN) and build config URI | `https://github.com/USA-RedDragon/talaria/.github/workflows/release.yaml@refs/tags/<tag>` (or `.../hermes-device/...`) |
| OIDC issuer | `https://token.actions.githubusercontent.com` |
| Source repository | `https://github.com/USA-RedDragon/talaria` (or `.../hermes-device`), ID <!-- docgen:const pkg=internal/attest name=RepoID code=false -->1452996986<!-- /docgen:const --> |
| Source repository owner | `https://github.com/USA-RedDragon`, ID <!-- docgen:const pkg=internal/attest name=OwnerID code=false -->13051767<!-- /docgen:const --> |
| Source ref | `refs/tags/<tag>`, the tag `update` installs |
| Event | `push` |
| Runner | `github-hosted` |
| Predicate type | `https://slsa.dev/provenance/v1` |
| Statement subject | a subject with the archive's file name and SHA-256, so that another archive of the same release does not pass |

6. One attestation that passes steps 4 and 5 is enough. Then `update` extracts the binary from the same archive in memory, checks that it runs and reports the release's version, and replaces the installed binary.

A release before v<!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const --> has no attestation, so `update` refuses it, also with `--force`, and says so.

The binary that runs `update` does the checks. A binary before v<!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const --> does not check attestations, so the update from it to the first attested release is checked only against the checksum file. Every update after that is verified.

`talaria update --check [version]` does steps 1 to 6 but installs nothing and restarts nothing. It also checks a release that is the same as or older than the installed one. It refuses `--insecure-skip-attestation`.

## 3. What it trusts

- The Sigstore public-good trusted root that is built into the binary: the Fulcio certificate authorities, the Rekor log key and the certificate transparency log keys. A release updates it when Sigstore rotates a key: replace `internal/attest/trusted_root.json` with the public-good line of `gh attestation trusted-root` (or `targets/trusted_root.json` of sigstore/root-signing), and update its date and SHA-256 in `internal/attest/trustedroot.go` and its test.
- GitHub Actions: its OIDC tokens say truthfully which repository, workflow, ref and runner a job runs for. Fulcio puts these claims in the certificate.
- The release workflow at the tag and the actions it pins by commit SHA, and everyone who can push a `v*` tag to the repository.

It does not trust the GitHub API or the release download: a changed API answer or file fails the signature, digest or policy check. They can only make an update fail.

The GitHub API allows 60 requests per hour without a token per IP address. `update` makes one API request per run.

## 4. Unsafe override

`talaria update --insecure-skip-attestation` installs without step 2 to 5. Then only the checksum file, which comes from the same place as the archive, vouches for the archive. Use it only in an emergency, for example when the GitHub attestations API is down, or to install a release before v<!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const -->. `update` prints a warning when you use it.

For tests only, the environment variable <!-- docgen:const pkg=internal/setup name=ReleasesURLEnv code=false -->TALARIA_TEST_RELEASES_URL<!-- /docgen:const --> replaces `https://github.com/USA-RedDragon/talaria/releases` for `setup` and `update`, so a test can serve a build that is not on GitHub from its own web server (`<url>/download/<tag>/<file>`). Such a build has no attestation, so `update` also needs `--insecure-skip-attestation`. Never set it on a computer you use.

## 5. Verify by hand

With the GitHub CLI:

```sh
gh attestation verify hermes-device_0.13.0_linux_amd64.tar.gz --repo USA-RedDragon/talaria \
  --signer-workflow USA-RedDragon/talaria/.github/workflows/release.yaml --source-ref refs/tags/v0.13.0
gh attestation verify oci://ghcr.io/usa-reddragon/hermes-device:v0.13.0 --repo USA-RedDragon/talaria
```

`gh attestation verify <file> --repo USA-RedDragon/talaria` alone checks that a workflow of the repository signed it. The two extra flags make it check the same workflow and tag as `update`. It also works for the checksum file and the `.xpi`.

## 6. Automatic updates

`talaria run` installs new releases by itself when the talaria service runs it, `update.auto` is true (the default) and the device token comes from `login` (README, Updates). Each check does this:

1. It gets the latest release from `GET /repos/USA-RedDragon/talaria/releases/latest` with the ETag of the last answer, or reuses an answer less than an hour old. It caches the answer in `release.json` in the user cache directory, which the CLI's notice also reads. After a 403 or 429 answer, nothing asks again until the time in `Retry-After` or `X-RateLimit-Reset` (at least a minute, at most a day). The cache is only a hint: a tag from it must still be a plain `vX.Y.Z`, and a value that cannot be right (a pre-release tag, a time in the future, a pause longer than a day) is dropped.
2. It goes on only when that release is a stable release (no pre-release flag, no pre-release or build part in the tag) and newer than the running binary.
3. It installs the release with the same code as `update`, steps 1 to 6 of section 2, with `--force` and `--insecure-skip-attestation` always off. Before it replaces the binary, it also runs the new binary with the daemon's arguments and `--check-config`, so a release that does not accept this computer's config is not installed. A release that fails a check is not installed, and the daemon tries again at the next check, not before.
4. It waits, for up to 30 minutes, until this device holds no command (running, or with output or an exit the plugin has not confirmed), runs no browser operation and no MCP call, and the chat splice has no TUI open. Then it refuses new work with a message that the device restarts for an update, and exits with status 75. systemd (`Restart=always`) and launchd (`KeepAlive`) start the new binary. When the device stays busy, the new binary stays installed and the daemon tries again at the next check.

The first check comes 1 to 5 minutes after `run` starts, and the next ones every `update.interval` (default 6h, at least 1h) with up to 10% jitter. So each daemon makes about 4 conditional API requests a day, plus the attestation request of section 2 when it installs a release.
