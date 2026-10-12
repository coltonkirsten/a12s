# Changelog

Breaking changes are allowed before 1.0 when they make a12s simpler; each one is listed here with what to do.

## Unreleased

### Changed
- **Presets follow their directory.** Edit a `home.preset` (say `presets/lead/CLAUDE.md`) and run `a12s up`: the
  files are copied onto the home drive at the Computer's next idle boundary and the session continues. `fleet.lock`
  updates the preset's entry instead of refusing the change. Before, a preset was copied once onto an empty home,
  and changing it needed a lock edit plus `a12s down --purge`.
  *Upgrade note:* every Computer with a preset restarts once at its next idle boundary after the upgrade.
