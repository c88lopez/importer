---
name: deploy-release
description: How importer gets to the cluster - Argo CD deploys from k8s/ on main, releasing a version with make release, rolling back, Secrets and the Postgres connection budget. Use when releasing, deploying, rolling back, changing k8s/ manifests, HPA maximums or pool sizes, or running make secrets.
---

# Deploy and release

## How a change reaches the cluster

- Argo CD deploys `k8s/` from `main`. The Application lives in the separate platform repo, not here.
- A commit that changes only `k8s/` (a ConfigMap, a dashboard) is live within a few minutes, with no release. Such commits skip CI.
- Check a deploy with `make status`, and `curl http://192.168.68.57:30080/readyz` (REST) or `:30082/readyz` (GraphQL).

## Releasing a version

All three services share one version: `newTag` in `k8s/kustomization.yaml`.

1. `make release VERSION=x.y.z` tags `vx.y.z` and pushes the tag. This is outward-facing: run it only when the user asks, with the version they gave.
2. CI builds one image per service from the tag.
3. Argo CD Image Updater commits the new `newTag` to `main` itself, as `argocd-image-updater`. Run `git pull` after a release before committing anything else.
4. The migration Job runs as a PreSync hook, so new migrations are applied before the new pods start.

## Rolling back

The cluster only rolls forward on schema; images can go back.

1. Add the bad tag to Image Updater's `ignoreTags` in the platform repo first.
2. Then set `newTag` back to the previous version on `main`.

A revert without step 1 gets undone: Image Updater sees the bad tag as the newest and commits it again.

## Secrets

Secrets aren't in Git. `make secrets` creates or updates each service's Secret from the Postgres, RabbitMQ and Redis Secrets in the `data` namespace.

## Postgres connection budget

Postgres allows 100 connections. The per-pod `pool_max_conns` (`API_POOL`, `WORKER_POOL` in the Makefile's `secrets` target) and the HPAs' `maxReplicas` are budgeted together. Raising either means redoing the arithmetic in the comment above that target, then running `make secrets` so the pods get the new URLs.
