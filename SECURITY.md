# Security policy

## Supported versions

codetrail has no tagged releases. Only the latest commit on `trunk` is supported, and fixes land
there.

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub's private vulnerability reporting: open the repository's
**Security** tab and choose **Report a vulnerability**, or go to
<https://github.com/mralaminahamed/codetrail/security/advisories/new>.

Include what you found, how to reproduce it and the commit you tested against. You will get a reply
on the advisory, and the fix and any advisory will be coordinated with you before anything is
published.

The README's [Indexing a stranger's repository](README.md#indexing-a-strangers-repository) section
states the known limits of the ingestion sandbox, including that the host allowlist is the only SSRF
control.
