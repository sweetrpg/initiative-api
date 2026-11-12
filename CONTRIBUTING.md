# Contributing

Thanks for considering a contribution to `initiative-api`.

## Branching

This repo follows the sweetrpg platform's git-flow convention:

* `develop` is the integration branch. All feature and fix branches merge here.
* `master` reflects the latest released state. Nothing is committed here directly.
* Branch names: `feature/<description>` for new functionality, `fix/<description>` for bug
  fixes, `hotfix/<description>` for urgent fixes to a released version.

```bash
git checkout develop
git pull
git checkout -b feature/my-change
# ... work, commit ...
git push -u origin feature/my-change
# open a PR: feature/my-change -> develop
```

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>
```

## Running checks locally

```bash
go build -v ./...
go vet ./...
go test -v -coverprofile coverage.out ./...
```

Full local run (with MongoDB) is available via `scripts/run-docker-local.sh` /
`docker/docker-compose.yml`.

## Pull requests

CI runs automatically on PRs targeting `develop`. Once checks pass and the PR is reviewed, it
can be merged (auto-merge is enabled once required checks pass).

## Releases

No release process yet - this repo has no tagged versions. Once the service is ready to deploy
past `dev`, add the `go-prepare-release.yaml`/`go-tag-release.yaml`/`go-release.yaml` reusable
workflows (see `docs/service-conventions.md` in the `platform` repo and a sibling like
`catalog-api`'s `RELEASE.md` for the pattern) rather than inventing a new one.
