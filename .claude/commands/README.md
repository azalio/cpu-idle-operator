# Claude Code Commands

This directory exists for **user-custom** slash commands. All MAP slash
commands now ship as Skills (`.claude/skills/map-*/SKILL.md`) which give
the same `/map-*` interface but with progressive disclosure (skill body
loads on demand instead of always living in context).

## MAP Slash Commands (skill-backed)

All of these are implemented via `.claude/skills/<name>/SKILL.md`:

- `/map-plan` - Decompose work without implementing it yet
- `/map-efficient` - Implement features with optimized workflow (recommended)
- `/map-fast` - Quick implementation with minimal validation
- `/map-task` - Execute a single subtask from an existing plan
- `/map-tdd` - Run a test-first workflow for one task or plan
- `/map-debug` - Debug issues using MAP analysis
- `/map-review` - Run a structured review workflow
- `/map-check` - Run workflow quality gates and verification
- `/map-release` - Execute MAP Framework package release workflow
- `/map-resume` - Resume an interrupted workflow from `.map/`
- `/map-learn` - Extract lessons from completed workflows

## Creating Custom Commands

Create a new `.md` file in this directory with the following format:

```markdown
---
description: Brief description of your command
---

Your command prompt here
```

The filename becomes the command name (without the `.md` extension).
Per the Claude Code docs, a skill at `.claude/skills/<name>/SKILL.md`
takes precedence over a command at `.claude/commands/<name>.md` with
the same name.
