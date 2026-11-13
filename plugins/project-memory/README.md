# project-memory

One curated memory per project, next to Hermes's global memory. A project is the git repository of the
folder a chat is bound to on a device (the Talaria device plugin's `/device <device> <dir>`), identified by its
normalized origin URL (`github.com/alice/demo`), else the upstream or only remote, else the oldest root commit;
a folder outside git is identified by device and folder. Clones, worktrees and mirrors on several computers
share one memory.

Provides:

- the `project_memory` tool (`read`, `write` with `add`, `patch`, `replace`, `remove` operations; same store rules
  and char limits as Hermes's own memory tool, also its write approval queue and background-review staging);
- a `project-memory` prompt section with the project's notes, frozen with the prompt like the global memory block;
- hub routes under `/api/plugins/project-memory/` for the Memory page (projects, notes, add/edit/remove/replace,
  staged writes);
- a note in the built-in `memory` tool description that sends project facts here.

Subagents can read project memory but not write it, as they cannot write the global memory.

## Config

```yaml
plugins:
  entries:
    project-memory:
      settings:
        char_limit: 3000   # optional; default: memory.memory_char_limit, at most 3000 (the prompt section's room)
        write_approval: false   # optional; true or false overrides memory.write_approval for project notes only; unset follows it
```

`write_approval: false` saves project notes directly while Hermes's own memory stays gated. `true` stages them even when `memory.write_approval` is off. Writes already staged under `/memory pending` stay staged; only new writes follow the setting. Background review still stages patch, replace and remove.

Notes live in `$HERMES_HOME/project-memories/<slug>/MEMORY.md`, with `meta.json` beside it.
