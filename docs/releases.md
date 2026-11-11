# Release verification

From v<!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const --> on, every release has GitHub artifact attestations, and `hermes-device update` installs a release only when the attestation of its archive verifies. This page says what is attested, what `update` checks, what it trusts, and how to check a file by hand.

## 1. What the release workflow attests

`.github/workflows/release.yaml` runs on a push of a `v*` tag. After the shared GoReleaser workflow has built and uploaded the release, and the `firefox-extension` job has uploaded the signed `.xpi`:

1. The `attest-files` job downloads the release's `.tar.gz` archives, its checksum file and the `.xpi`, checks the archives against the checksum file, and attests all of them with `actions/attest-build-provenance`. That is one SLSA build provenance v1 statement with one subject per file.
2. The `attest-images` job finds the digest of the image index of `ghcr.io/usa-reddragon/hermes-device:<tag>` and `ghcr.io/usa-reddragon/hermes-device-sandbox:<tag>` and attests each digest. It also pushes the attestation to GHCR next to the image.

The attestation is signed with a short-lived certificate from the Sigstore public-good Fulcio. The certificate names the workflow run: the workflow file and ref, the repository and its numeric ID, the owner and its numeric ID, the commit, the event and the runner type. The signature is recorded in the Rekor transparency log. GitHub stores the attestation; anyone can read it with `GET /repos/USA-RedDragon/hermes-device/attestations/sha256:<digest>`.

The jobs attest the files as the release holds them after the upload, and the image that the tag names at that time. Someone who can write to the repository's releases or packages in the minutes between the upload and the attestation could swap a file first. Both jobs refuse a release that was published more than 3 hours before, so a re-run of the job cannot attest a file that was changed later in an old release. Inside that window the attestation does not protect against a repository write token: such a token can also push a tag and run the release workflow. It protects against a release file or checksum file changed after that window, a changed download on the way, and a mirror.

## 2. What `update` checks

`update` does these steps in order and stops at the first that fails. It installs nothing when one fails.

1. It downloads the checksum file and the archive for this OS and architecture, and checks the archive's SHA-256 against the checksum file. The archive stays in memory from here on.
2. It gets the attestations for that SHA-256 from the GitHub API (only `https://slsa.dev/provenance/v1` statements, up to 5 pages of 30). No attestation at all fails.
3. It reads the Sigstore trusted root of the public-good instance from its TUF repository (`tuf-repo-cdn.sigstore.dev`), checked against the TUF root that sigstore-go builds in. It caches the TUF metadata for a day in the user cache directory (`~/.cache/hermes-device/sigstore-tuf` on Linux, `~/Library/Caches/hermes-device/sigstore-tuf` on macOS).
4. For each attestation, it verifies the Sigstore bundle with sigstore-go, the same way `gh attestation verify` verifies a public-good bundle: the certificate chains to the Fulcio root, has a signed certificate timestamp, and was valid when the transparency log recorded the signature; the log entry verifies against the Rekor key; the DSSE signature verifies with the certificate's key; and the statement has a subject with the archive's SHA-256.
5. It checks the certificate against this policy. Every item must match exactly:

| Field | Required value |
| --- | --- |
| Subject (SAN) and build config URI | `https://github.com/USA-RedDragon/hermes-device/.github/workflows/release.yaml@refs/tags/<tag>` |
| OIDC issuer | `https://token.actions.githubusercontent.com` |
| Source repository | `https://github.com/USA-RedDragon/hermes-device`, ID <!-- docgen:const pkg=internal/attest name=RepoID code=false -->1452996986<!-- /docgen:const --> |
| Source repository owner | `https://github.com/USA-RedDragon`, ID <!-- docgen:const pkg=internal/attest name=OwnerID code=false -->13051767<!-- /docgen:const --> |
| Source ref | `refs/tags/<tag>`, the tag `update` installs |
| Event | `push` |
| Runner | `github-hosted` |
| Predicate type | `https://slsa.dev/provenance/v1` |
| Statement subject | a subject with the archive's file name and SHA-256, so that another archive of the same release does not pass |

6. One attestation that passes steps 4 and 5 is enough. Then `update` extracts the binary from the same archive in memory, checks that it runs and reports the release's version, and replaces the installed binary.

A release before v<!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const --> has no attestation, so `update` refuses it, also with `--force`, and says so.

The binary that runs `update` does the checks. A binary before v<!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const --> does not check attestations, so the update from it to the first attested release is checked only against the checksum file. Every update after that is verified.

`hermes-device update --check [version]` does steps 1 to 6 but installs nothing and restarts nothing. It also checks a release that is the same as or older than the installed one. It refuses `--insecure-skip-attestation`.

## 3. What it trusts

- The Sigstore public-good TUF root that is built into the binary (through sigstore-go), and what that TUF repository signs: the Fulcio certificate authority, the Rekor log key and the certificate transparency log key.
- GitHub Actions: its OIDC tokens say truthfully which repository, workflow, ref and runner a job runs for. Fulcio puts these claims in the certificate.
- The release workflow at the tag and the actions it pins by commit SHA, and everyone who can push a `v*` tag to the repository.

It does not trust the GitHub API or the release download: a changed API answer or file fails the signature, digest or policy check. They can only make an update fail.

The GitHub API allows 60 requests per hour without a token per IP address. `update` makes one API request per run.

## 4. Unsafe override

`hermes-device update --insecure-skip-attestation` installs without step 2 to 5. Then only the checksum file, which comes from the same place as the archive, vouches for the archive. Use it only in an emergency, for example when the GitHub attestations API or the Sigstore TUF repository is down, or to install a release before v<!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const -->. `update` prints a warning when you use it.

## 5. Verify by hand

With the GitHub CLI:

```sh
gh attestation verify hermes-device_0.13.0_linux_amd64.tar.gz --repo USA-RedDragon/hermes-device \
  --signer-workflow USA-RedDragon/hermes-device/.github/workflows/release.yaml --source-ref refs/tags/v0.13.0
gh attestation verify oci://ghcr.io/usa-reddragon/hermes-device:v0.13.0 --repo USA-RedDragon/hermes-device
```

`gh attestation verify <file> --repo USA-RedDragon/hermes-device` alone checks that a workflow of the repository signed it. The two extra flags make it check the same workflow and tag as `update`. It also works for the checksum file and the `.xpi`.
