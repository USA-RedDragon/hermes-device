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

## Project skills

With the Talaria Hermes patch `0094-project-skills`, the project also has skills, kept next to its notes in
`$HERMES_HOME/project-memories/<slug>/skills/` (same slug, so clones, worktrees and mirrors share them). The plugin
registers the project of each session with Hermes; `skill_manage` then takes `scope: project` or `scope: global`.

- A project skill is listed (marked `[project]`), loaded and edited only in chats bound to that project and in their
  subagents, and wins over a global skill of the same name there. It needs no trust entry.
- Project skills hold knowledge about the one codebase: layout, commands, people, conventions, and a project-specific
  lesson found while using a global skill. A global skill is only for a procedure that applies across projects and is
  written generically: no project names, repo paths, hostnames or usernames. The prompt block, the `skill_manage`
  description and the memory tool description say so, and a global skill that names the project gets a warning.
- A chat with no project keeps every skill global, as before. On a Hermes without the patch nothing changes.
- With the device plugin's skill mirror configured, project skills are committed to the same repository under
  `projects/<slug>/<skill>`.
- The hub's project list carries `skills`, the number of project skills.
- Hub routes (under `/api/plugins/project-memory`): `GET /projects/<slug>/skills` lists them (name, folder, description,
  on or off), `GET /projects/<slug>/skills/<name>` returns one with its SKILL.md text, `DELETE` removes it.

`scripts/audit-global-skills.py` (in the plugins directory) lists global skills that look project-specific, as a
read-only report.

## Config

```yaml
plugins:
  entries:
    project-memory:
      settings:
        char_limit: 8000   # optional; default: memory.memory_char_limit; either is held between 200 and 32000
        write_approval: false   # optional; true or false overrides memory.write_approval for project notes only; unset follows it
```

`write_approval: false` saves project notes directly while Hermes's own memory stays gated. `true` stages them even when `memory.write_approval` is off. Writes already staged under `/memory pending` stay staged; only new writes follow the setting. Background review still stages patch, replace and remove.

The prompt holds the notes up to `char_limit` (plus a short header), as far as Hermes allows a plugin section. Hermes caps a section at 4,000 characters unless `plugins.max_section_chars` is raised in its config, so with `char_limit: 8000` set `plugins.max_section_chars` to 9000 or more (the plugin asks for at most 33,000; all sections together may use that value plus 4,000). The plugin logs a warning at load when the allowed size is below the limit. When the notes are longer than the section, the prompt shows the entries that fit, in order, and says how many it left out; the agent reads the rest with `project_memory action read`. Keep the entries the agent needs in every turn first.

```yaml
plugins:
  max_section_chars: 12000
```

This setting needs the Talaria Hermes patch `0091-plugin-section-max-chars`; a Hermes without it keeps 4,000.

Notes live in `$HERMES_HOME/project-memories/<slug>/MEMORY.md`, with `meta.json` beside it.
