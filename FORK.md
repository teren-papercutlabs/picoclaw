# Fork: picoclaw

**Upstream**: https://github.com/sipeed/picoclaw
**PcL fork**: https://github.com/teren-papercutlabs/picoclaw
**Fork point**: c308ebb97c133e7aee65c33d8746d26d62dccd70 (tag: upstream-fork-point)
**Last sync**: 2026-03-18 (initial fork)

## Customization philosophy

PcL adds client deployment features (cost tracking, telemetry, health endpoints) without modifying upstream files where possible. All PcL changes use the `pcl:` commit prefix. New files in `pcl/` package preferred over inline upstream modifications.

## Commit convention

- `pcl:` — permanent PcL customization
- `fix:` — workaround, may be superseded by upstream
- `chore:` — build/CI, probably temporary
- `drop:` — intentionally removing an upstream feature

Trailers: `Downstream: permanent | temp-fix | propose-upstream`

## Sync procedure

See fork-maintenance skill (shared-agent-memory/skills/fork-maintenance/reference/rebase-procedure.md)
