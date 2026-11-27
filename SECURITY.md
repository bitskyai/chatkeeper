# Security policy

Please report vulnerabilities privately using [GitHub private vulnerability
reporting](https://github.com/bitskyai/chatkeeper/security/advisories/new).
Do not include credentials or private archives in public issues. If reporting is
unavailable, ask a maintainer to enable it without disclosing exploit details.
Maintainers must enable private reporting before public launch.

Security fixes target the latest release. Pre-1.0 releases may require an upgrade
rather than a backport. Include affected versions, reproduction steps using
synthetic data, impact and any suggested mitigation. We aim to acknowledge reports
within seven days; this is a best-effort project, with no guaranteed response SLA.

ChatKeeper stores sensitive chat history. Protect archives and backups, restrict
filesystem access, and put server deployments behind TLS and access controls.
Read-only API tokens grant access to every configured workspace. Compiled plugins
are trusted in-process code. See the README for the privacy and security model.
