# dotagents

My portable agent home: the writing ruleset (`AGENTS.md`) and the general-purpose skills I want on every machine and in every harness (Claude Code, opencode, pi, codex). Single source, symlinked everywhere.

## Use as ~/.agents

```sh
git clone https://github.com/fagerbergj/dotagents ~/.agents
~/.agents/setup.sh
```

`setup.sh` runs `setup-symlinks.sh` then `install-plugins.sh` (Claude Code plugins, currently just ponytail); both work standalone. The symlinks put `AGENTS.md` and `skills/` in each harness's expected location:

| Harness | Instructions | Skills |
| --- | --- | --- |
| Claude Code | `~/.claude/CLAUDE.md` | `~/.claude/skills` |
| opencode | `~/.config/opencode/AGENTS.md` | reads `~/.agents/skills` natively |
| pi | `~/.pi/agent/AGENTS.md` | `~/.pi/agent/skills` |
| codex | `~/.codex/AGENTS.md` | n/a |

Existing real files are backed up to `<path>.bak` before linking; existing symlinks are replaced.

## Use in a project

```sh
git submodule add https://github.com/fagerbergj/dotagents .agents/vendor/dotagents
```

Point the project's skill loader at `.agents/vendor/dotagents/skills`, or symlink individual skills into the project's own skills directory.

## Layout

- `AGENTS.md` - writing ruleset, applied to prose the agent writes for me.
- `skills/<name>/SKILL.md` - one skill per directory, with optional `references/` and `assets/`.

[ponytail](https://github.com/DietrichGebert/ponytail) is deliberately NOT vendored here - install it as a Claude Code plugin (`install-plugins.sh` does this) so it updates through the plugin system. Projects that need its skills on disk for other harnesses (e.g. quack's opencode agents) vendor it themselves.
