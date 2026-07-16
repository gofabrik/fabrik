# Releasing fabrik

Every published module uses the same release version and is tagged together,
for example `jobs/v0.2.0`, `router/v0.2.0`, and `fabrik/v0.2.0`.

Minor pre-1.0 releases may include breaking changes.

Run commands from the repository root with `task`, `go`, `git`, `jq`, and `gh`
installed.

## Before you start

- Clean working tree, on `main`, `task ci` green.
- Pick the version. Bump the minor for breaking changes (`v0.1.0` -> `v0.2.0`).
- Set it in `versions.yaml` (`module-sets.fabrik.version`) and commit.

## Cut a release

1. Prepare the prerelease branch. The first task switches to it; run the
   remaining commands there:

   ```
   task release:prerelease VERSION=v0.2.0
   task release:converge
   task changelog:update VERSION=v0.2.0
   ```

2. Commit on that branch and open the PR:

   ```
   git add -A && git commit -m "release: v0.2.0"
   git push -u origin prerelease_fabrik_v0.2.0
   gh pr create --base main --label release --title "Release v0.2.0"
   ```

3. Review the PR: check the new `CHANGELOG.md` section and the version/require
   bumps. Merge it.

4. Tag the merge with the **Release (tag)** workflow using the version and merge
   SHA. It validates the release commit, builds four CLI archives before
   tagging, pushes the module tags, and publishes the archives and checksums to
   the `fabrik/<version>` GitHub release.

   For local tagging, fetch the merge commit first:

   ```
   git checkout main && git pull
   task release:tag VERSION=v0.2.0 COMMIT=<merge-sha>
   ```

   The commit must exist locally with a `versions.yaml` matching the current
   checkout. The task checks version agreement and tag conflicts, but neither
   verifies `main` membership nor publishes binaries. For local assets, run
   `task release:build-binaries` before creating the release with `gh`.

Steps 1-2 can also run as the **Release (prepare)** workflow (Actions tab) with
the version, which pushes the branch and opens the PR for you.

## Changelog fragments

Every behavior-changing PR adds one:

```
task changelog:new FILE=short-slug
```

Fill in the created `.chloggen/short-slug.yaml` (change_type, component, note,
issue/PR number). `task changelog:validate` checks it. Fragments collapse into
`CHANGELOG.md` at release.

## First release

Before `v0.1.0` is published:

- Until `v0.1.0` is tagged, `fabrik new` writes no fabrik requires and
  `GOWORK=off` builds cannot resolve the modules. Both work after tags are
  published.
- The go.mod files already use `v0.1.0`, so `prerelease` primarily creates the
  release branch.

## Troubleshooting

- **converge fails**: run `task manifest:fix` for missing requirements, commit,
  and retry.
- **"tag already exists"**: use a new version.
- **commit not found locally**: run `git checkout main && git pull`, then retry.
- **workspace stops building after a bump**: the `go.work` replaces are pinned
  to the version. Regenerate them with `task workspace:sync`.

## Repository setup

- Turn on branch protection for `main` (require CI + review).
- Add a `RELEASE_TOKEN` secret using a PAT or GitHub App token with `contents`
  and `pull-requests` write access. Without it, approve the generated PR's CI
  workflow runs before merging.
- The workflow actions are pinned to commit SHAs; refresh them when you bump the
  action versions.
