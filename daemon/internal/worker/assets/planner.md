<!-- Prompt template for the headless planning run. Rendered by internal/worker/planner. -->

A player of the Minecraft modpack "{{pack_name}}" (Minecraft {{mc_version}},
{{loader_id}} {{loader_version}}) made this request:

Player {{player_name}} says:

"""
{{request_text}}
"""
{{clarification_qa}}

Fulfill the request against this working copy, following the rules in CLAUDE.md:

1. Inspect the pack first: run `jarjard modtool installed`, and read any config files
   relevant to the request.
2. If mods must be added/updated/removed, find them with `jarjard modtool search` and
   pick a concrete version with `jarjard modtool versions`. Choose the newest compatible
   version. Prefer well-maintained, popular mods when several match; prefer Modrinth.
3. If configuration must change, edit the files directly, minimally.
4. Answer with the plan JSON: all mod actions with exact IDs from modtool output, one
   `config_edit` per file you touched, `server_property` actions for server settings, a
   player-friendly `summary`, technical `admin_notes`, and your `confidence`
   (`high` = clearly the right change; `medium` = reasonable interpretation involved;
   `low` = significant guesswork — the admin will review before it ships).
