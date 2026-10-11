# Security policy

## Supported versions

| Version | Security fixes |
|---|---|
| Latest 1.x minor release | Yes |
| Older 1.x minors and 0.x | No; upgrade to the latest release |

## Reporting a vulnerability

Report it privately through GitHub: **Security → Report a vulnerability**
on this repository (private vulnerability reporting). Please don't open a
public issue, pull request or discussion about it.

Include what you found, how to reproduce it (a minimal config with
example.com addresses, never real mail or credentials), the version
(`imap-mcp version`) and the impact you expect.

- **First response:** within 72 hours.
- **Critical issues:** a fixed release within 14 days of confirmation.
- Others are fixed in the next release. You'll be credited in the advisory
  unless you'd rather not be.

## How the project stays secure

- Every pull request to `main` must pass tests (with the race detector),
  golangci-lint, gosec, govulncheck, a gitleaks scan of the full history
  and dependency review. Repository admins can bypass in an emergency.
- Vulnerable dependencies are upgraded when a fix exists. When none does,
  the finding is traced against this code and recorded with its analysis in
  [security/accepted-risks.yml](security/accepted-risks.yml); a daily
  workflow re-checks every entry and opens a PR as soon as a fix is
  published.
- Releases are built by GoReleaser from an exact `vX.Y.Z` tag after the same
  checks. `SHA256SUMS` is signed keylessly with cosign (Sigstore), each
  archive has an SBOM, and GitHub records build provenance. To verify:

  ```bash
  cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
    --certificate-identity-regexp '^https://github.com/dmz006/imap-mcp/.github/workflows/release.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  sha256sum --ignore-missing -c SHA256SUMS
  gh attestation verify imap-mcp_<version>_linux_amd64.tar.gz --repo dmz006/imap-mcp
  ```

## Security considerations when running imap-mcp

- **Mail credentials** belong in a secret store or environment variables
  (`${secret:name}`, `${ENV}`), never in a committed config file.
- **Local state** (`imap.db`, `cache.db`) holds mail metadata and cached
  bodies; turn on encryption at rest ([docs/encryption.md](docs/encryption.md)).
- **The HTTP server** binds to localhost by default and requires bearer
  tokens with scopes ([docs/auth-tokens.md](docs/auth-tokens.md)). Give each
  client only the scopes it needs; `send` and `unsubscribe` act outside your
  mailbox.
- **Mail is untrusted input.** Model prompts fence message bodies, the inbound
  command channel is default-deny ([docs/datawatch-integration.md](docs/datawatch-integration.md)),
  and unsubscribe never follows links in a message ([docs/unsubscribe.md](docs/unsubscribe.md)).
