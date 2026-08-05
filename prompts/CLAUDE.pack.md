<!-- Rendered into each job worktree as CLAUDE.md. Placeholders filled by the worker. -->

# {{pack_name}} — modpack working copy

You are operating inside a **git worktree of a Minecraft modpack** for
Minecraft {{mc_version}} with the **{{loader_id}} {{loader_version}}** loader.
Your job is defined by the prompt; these are the standing rules of this workspace.

## What is here

- `jarjar.pack.json` — pack metadata (read-only for you).
- `mods.lock.json` — the authoritative list of installed mods (read-only for you; mods
  are added/removed via plan actions, never by editing this file).
- `config/`, `defaultconfigs/`, `kubejs/`, `scripts/` — mod configuration and scripts.
  **These are the only paths you may create or edit.**
- `.jarjar/server.properties` — read-only snapshot of the live server settings. To
  change a server property, emit a `server_property` action; do not edit any file for it.
- Mod jar files are not present in this workspace. That is normal.

## Registry access

Your only source of truth about available mods is the `jarjard modtool` command:

```
jarjard modtool search "<query>"            # find mods (pre-filtered to this MC version + loader)
jarjard modtool project <platform> <id>     # details for one project
jarjard modtool versions <platform> <id>    # concrete installable versions + dependencies
jarjard modtool installed                   # what this pack already has
```

Every `project_id` / `version_id` you put in the plan MUST be copied verbatim from
`modtool` output in this session. Never invent, recall from memory, or abbreviate IDs —
all of them are re-verified and the whole job is rejected on any mismatch.

## Config edits

- Before editing a config file, read it. Only change keys that exist or that you have
  concrete evidence the mod supports (e.g. a default config shipped in this workspace).
  Never invent config keys.
- Preserve file format, comments, and formatting; make the smallest change that
  fulfills the request.
- Every file you modify must be listed in your final plan as a `config_edit` action —
  a mismatch between your edits and your plan rejects the job.

## Output

Your final answer must be the JSON plan (the harness enforces its schema). Rules:

- If the request is ambiguous in a way that materially changes the outcome (e.g. several
  plausible mods), set `status: "needs_clarification"` with ONE short question listing
  the options. Do not guess between meaningfully different alternatives.
- If the request cannot be fulfilled for this MC version/loader, set
  `status: "infeasible"` and explain in `summary`.
- `summary` is shown to non-technical players: one to three plain sentences, no file
  paths, no IDs.
- Prefer the smallest change that satisfies the request. Do not refactor, reorganize,
  or "improve" anything that was not asked for.
