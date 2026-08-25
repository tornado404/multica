---
name: sync-upstream
description: Sync this ZCode fork with upstream multica-ai/multica and rebuild the local Desktop GUI + daemon CLI. Use when asked to "update from upstream", "sync with the original repo", "pull in the new multica release", or after an upstream release/tag lands.
metadata:
  version: "1.0.0"
---

# Sync fork with upstream + rebuild local Desktop/CLI

This fork (`feat/zcode-app-runtime` on origin `tornado404/multica`) carries the
ZCode runtime integration on top of `upstream` (`multica-ai/multica`).
Updating means: rebase the fork onto the new upstream tip, then rebuild and
reinstall the two artifacts this machine runs — the `multica` daemon CLI and
the Multica.app desktop bundle (which embeds its own copy of the CLI).

## Run the script

```bash
bash scripts/sync-upstream.sh                  # rebase onto upstream/main + rebuild + reinstall
bash scripts/sync-upstream.sh --base v0.4.33   # pin to a release tag instead
bash scripts/sync-upstream.sh --skip-rebase    # after manually resolving conflicts
```

The script rebase-stops on conflict and exits 3. Do NOT blindly take either
side — apply the fork conventions below, `git rebase --continue` until done,
then re-run with `--skip-rebase` to finish verification/build/install.

## Conflict-resolution conventions

Nearly every conflict is one of these shapes. Resolve in this order of
preference: union > upstream > fork.

1. **Runtime/provider lists** (SupportedTypes, launchHeaders, agents probes,
   `defaultAgentCommandNames`, i18n landing copy, docs tables, logo switch
   cases): take the **union** — upstream's new runtimes (dsh, mcode, dim,
   zeroclaw, …) plus `zcode`. Watch for:
   - Go map literals and sorted `.txt` lists: duplicates are compile/logic
     errors — dedupe.
   - `New()`'s default error message: keep upstream's `strings.Join` form.
   - Landing-page copy counts ("N supported coding tools"): the count must
     equal the list length after the union (recount, don't trust either side).
   - Docs paragraphs that both sides rewrote: keep upstream's version and
     append the fork's ZCode-specific sentences; drop duplicated lead-ins.

2. **Migration numbering** (the most common hard failure): the fork's
   `server/migrations/<N>_runtime_profile_add_zcode.{up,down}.sql` must be
   renumbered to **upstream_max + 1** whenever upstream has taken `N`. After
   renaming:
   - The `.up.sql` CHECK whitelist = upstream's *latest* whitelist + `zcode`
     (copy the list from upstream's highest `*_runtime_profile_add_*` file).
   - The `.down.sql` = upstream's latest whitelist unchanged.
   - Update the migration-number comment in `server/pkg/agent/agent.go`
     (SupportedTypes doc) and pin sets in
     `server/pkg/agent/agent_supported_types_test.go`.
   - Historical "renumber to NNN" fork commits become empty → `git rebase
     --skip` them.

3. **Upstream refactors of shared signatures**: upstream may change helpers
   the fork calls (e.g. `ListModels` now takes a `Command`, discovery
   helpers take `runtimeCmd Command` not `executablePath string`). Adapt
   fork call sites to the new signature; keep fork logic unchanged.

## Machine-specific install facts (what the script automates)

- CLI installs to `~/.multica/bin/multica` (Desktop runtime copy) and to
  `$(realpath $(command -v multica))` — on this machine a brew Cellar
  binary that has been overwritten with fork builds (never `brew upgrade`).
- Desktop is packaged unsigned/ad-hoc: `CSC_IDENTITY_AUTO_DISCOVERY=false
  pnpm --filter @multica/desktop run package -- --mac --arm64` (this also
  compiles the Go CLI into the bundle). Old app is kept as
  `/Applications/Multica.app.bak-<version>`.
- pnpm/node live under nvm; non-interactive shells need
  `PATH="$HOME/.nvm/versions/node/<latest>/bin:$PATH"`.
- Daemons restart per profile — check for active agent tasks BEFORE
  restarting: `multica [--profile <name>] daemon logs | tail`, then
  `multica [--profile <name>] daemon restart`. Profiles live under
  `~/.multica/profiles/`.

## After the sync

- Run the script's checks (go test ./pkg/agent/ ./internal/daemon/,
  pnpm typecheck) — they must pass before installing.
- `pnpm install` is required when upstream added workspace packages
  (typecheck fails with TS2307 on fresh packages otherwise).
- The branch history is rewritten by the rebase; push with
  `git push --force-with-lease` only when the user asks.
