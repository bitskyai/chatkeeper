# Public launch checklist

M4 prepares release automation. It does not authorize a tag, merge, visibility
change, history rewrite or production migration. Maintainers must complete the
unchecked gates before public launch.

## Audit receipt (2026-11-27)

- [x] Full local history scan: `go run github.com/zricethezav/gitleaks/v8@v8.30.1 git --redact --no-banner --log-opts=--all --report-format json --report-path /tmp/chatkeeper-gitleaks.json`.
  Result: 29 commits, zero leaks in the M4 review scan (including baseline 7260bac).
- [x] `git log --format='%ae %ce' | sort -u`: two distinct author/committer pairs;
  one contains a personal Gmail author address, the other uses GitHub noreply.
  The exact address remains in private git history and is deliberately not
  reproduced in launch documentation.
- [ ] Publish sanitized history to a **new repository** using the procedure below.
  Never flip this repository's visibility. Six Gmail-authored squash commits
  (#9–#18, 0867003 through 7260bac) remain reachable through GitHub-managed
  `refs/pull/*/head`, and alpha tags/releases also retain the old history.
  Rewriting branches in place cannot remove those refs.
- [x] Tracked-tree grep for `/home/ubuntu`, `/srv/`, known internal Slack ID prefixes,
  the historical personal email, and `ai-lab`: no hits before adding this receipt.
  Re-run with organization-specific patterns before publication. Generic synthetic
  fixture IDs are intentional. Full-history `git grep` across all 29 revisions for the internal path and ID patterns also found zero files; commit-message grep found zero matches. Repeat on sanitized public refs.
- [ ] Independent review of full-history internal IDs, hostnames and paths. A
  pattern scan cannot establish that every possible private identifier is absent.
- [x] MIT license exists; community policies, issue/PR templates and Dependabot added.

## Release and deployment gates

- [ ] Protect main: PR review, required CI, no force pushes; restrict release tags
  and review changes to workflows before allowing publication tokens.
- [ ] Remove this repository from the bitskyai ARC runner scale set / runner
  group's selected repositories before public launch. Ensure the new public
  repository has no access to any organization or repository self-hosted runner.
  A fork can edit its own workflow's `runs-on`; a workflow conditional is no
  security boundary. Delete the obsolete CI_RUNNER repository variable.
- [ ] Actions → General → Fork pull request workflows: require approval for
  **all outside collaborators** (including returning contributors). Confirm the
  effective organization policy also enforces this on the new repository.
- [ ] Settings → Environments → create `release`, add maintainer required
  reviewers, prevent self-review and disallow administrator bypass. Restrict
  deployment refs to protected release tags. Both publishing jobs use this
  environment; the container job runs only after binary release succeeds.
- [ ] Settings → Rules → Rulesets → New tag ruleset: target `v*`, enforcement
  Active, restrict creations, updates and deletions; add only the designated
  maintainer team as bypass actors. Do not grant a general Write-role bypass.
  Confirm a non-maintainer cannot create a release tag and an unapproved release
  cannot publish. Environment YAML alone does not configure required reviewers.
- [ ] Enable GitHub private vulnerability reporting and security advisories.
- [ ] Ensure GHCR package visibility and inherited permissions match public intent.
- [ ] Confirm all PR jobs use GitHub-hosted runners and contain no secret inputs.
- [ ] Run M4 CI and local packaging checks recorded in the PR. Review Syft SBOMs.
- [ ] Create an authorized release-candidate tag only after merge. Confirm six
  archives, SHA256SUMS, six SBOMs and GitHub attestations on GitHub Releases.
- [ ] Download each published archive; verify SHA256SUMS and `gh attestation verify
  <archive> --repo bitskyai/chatkeeper`. Tag CI checks checksums; public verification
  against the actual downloaded artifacts remains a launch gate.
- [ ] Clean-machine download → run → loopback wizard smoke on macOS, Ubuntu and
  Windows, including arm64 where available. Native CI tests are not this rehearsal.
- [ ] Pull and run both image architectures; verify image provenance, non-root
  ownership, volume persistence, restart and capture. Prereleases never update latest.
- [ ] Compose first-run browser setup with one-time code, followed by persistence
  and restart checks. Default Compose uses environment admin bootstrap.
- [ ] Helm kind install, Secret/bootstrap, probes, PVC restart and upgrade rehearsal.
- [ ] Optional cosign keyless signing: not enabled. If enabled later, sign checksums
  and image digests and exercise `cosign verify-blob`/`cosign verify` in tag CI with
  the exact repository workflow identity and GitHub OIDC issuer.

## Migration and publication gates

- [ ] `migrate-from-threadkeep` automation from issue #4/design section 15 remains
  outstanding; this release packaging branch does not implement that command.
- [ ] Rehearse against a copy of real threadkeep data: hash inventories before and
  after verification, config/env mapping review, search/viewer parity and rollback.
- [ ] Complete shadow/parallel capture comparison and backup restore rehearsal.
- [ ] Review unsigned macOS/Windows launch instructions on clean machines.
- [ ] Approve sanitized public history, rerun all audits, then explicitly authorize
  creation of the new public repository. Keep the original repository private.
- [ ] Explicitly authorize the first public tag/release and announce it.

## Fresh public repository procedure (maintainer execution only)

These steps are a launch plan, not authorization to execute them during M4.

1. In GitHub account email settings enable **Keep my email private** and **Block
   command line pushes that expose my email**. Set local author and committer
   email to the account's GitHub noreply address and choose that address in every
   squash-merge dialog. Confirm every maintainer does the same.
2. Finish reviewed development in the current private repository. Record the
   approved source commit and back up the private repository. Freeze merges and
   tags during the transition. Never change this repository to public.
3. Rename the current private repository to `bitskyai/chatkeeper-dev` using
   Settings → General → Repository name. Verify it remains private; restrict its
   runner access as above. Its PR refs, alpha releases, issues and old history
   stay private there. Update private development remotes explicitly.
4. Prepare sanitized history in an empty local directory. The single initial
   commit route avoids copying any private git objects or refs:
   ```sh
   # Run in the approved private source checkout; substitute the reviewed SHA.
   git archive --format=tar APPROVED_SHA > /tmp/chatkeeper-public-tree.tar
   mkdir /tmp/chatkeeper-public
   cd /tmp/chatkeeper-public
   git init -b main
   git config user.name 'MAINTAINER_NAME'
   git config user.email 'ACCOUNT_ID+LOGIN@users.noreply.github.com'
   tar -xf /tmp/chatkeeper-public-tree.tar
   git add .
   git commit -m "Initial public release"
   ```
   Substitute real maintainer noreply identity before committing. Alternatively,
   rewrite history in an isolated clone with all author/committer emails changed
   to approved noreply identities; push only audited branches, never mirror refs.
   Never copy `.git`, PR refs, old tags or old release assets into the new repo.
5. Audit the complete intended public tree and history: repeat gitleaks with
   `--all`, inspect `git log --all --format='%an <%ae> %cn <%ce>'`, and repeat
   internal identifier/path/hostname and commit-message scans. Obtain independent
   review. Confirm the six original squash commits are absent from reachable
   history. Record a new audit receipt with the actual commit count.
6. Create a **new private** `bitskyai/chatkeeper` repository with no README,
   license or template initialization. This new repository must have a different
   repository ID from `chatkeeper-dev`. Disable Actions temporarily, then push
   only sanitized main:
   ```sh
   git remote add origin git@github.com:bitskyai/chatkeeper.git
   git push -u origin main
   ```
   Do not use `--mirror`, `--all` or `--tags`. Creating the new name replaces
   GitHub's old-name redirect; check all development remotes afterward.
7. Recreate protections on the new repository: main branch review/required CI,
   the `v*` maintainer-only tag ruleset, the `release` environment with required
   reviewers and tag restrictions, outside-contributor workflow approval, and
   no ARC/self-hosted runner access. Settings, environments, rulesets and secrets
   do not transfer from the old repository. Review Actions token permissions,
   GHCR inherited access/visibility and any required secrets explicitly.
8. Enable Actions, run CI on sanitized main, and verify all gates above on the
   new repository. Set its default branch to main. Obtain explicit authorization
   to make **only the new repository** public, then change its visibility and
   recheck environment protections, rulesets, runner access and contributor
   approvals. The renamed `chatkeeper-dev` must remain private permanently.
9. Only after separate release authorization, create the first new `v*` tag
   from audited public main, approve the gated jobs, and perform the release
   artifact/image/native-machine rehearsals above. Do not import the old alpha
   tags or releases.
