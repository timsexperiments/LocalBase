# Security policy

## Supported versions

Only the latest release is supported with security fixes. LocalBase is pre-1.0, and there are no long-term support branches.

## Reporting a vulnerability

Report privately through [GitHub Security Advisories](https://github.com/timsexperiments/LocalBase/security/advisories/new). Please don't open a public issue. Include the affected version, how it's deployed and the steps to reproduce, without live credentials.

The project has one volunteer maintainer, so there's no guaranteed response time. Please allow reasonable time for a fix before publishing details.

## In scope

- Gateway authentication and authorization: API keys, scopes, browser sessions, OIDC, Cloudflare Access, magic links.
- CSRF, and anything that lets one user or key act beyond its permissions.
- Secrets or prompt content reaching logs, diagnostics bundles or telemetry.
- Exposure of the gateway or runtimes beyond their configured address.
- Download integrity: checksums and pins for runtimes, models and releases.
- The install script and release artifacts.

## Out of scope

- Vulnerabilities in third-party runtimes or model weights. Please report those upstream, and tell us if LocalBase needs a new pin.
- What a model says, including jailbreaks and unsafe output.
- Attacks that need write access to the data directory or the same user account: that access already means full control.
- Denial of service by an authenticated user with inference access.
