# Bundle layout

A skill bundle is a directory. `skill:save` reads it from your session
workspace and registers it. Layout:

```
<bundle-dir>/
  SKILL.md            # required — the manifest + body
  references/         # optional — markdown docs the body cites
    howto.md
    deep/dive.md      # subdirs allowed
  scripts/            # optional — executables the body runs
    report.py
  assets/             # optional — text data files (templates, etc.)
    template.html
```

Rules:

- `SKILL.md` is mandatory; everything else is optional.
- Only `references/`, `scripts/`, `assets/` are read into the bundle.
  Any other files in the directory (scratch notes, research files you
  wrote while building) are IGNORED — they stay in your workspace and
  are not registered.
- Paths inside the three subdirs may nest. No leading `/`, no `..`,
  no hidden (dot) segments — those files are skipped.
- v1 stores text only; binary assets are not supported.

## Building it

Write the files with the bash / filesystem tools. Relative paths
resolve against your session workspace (the same place
`bash.read_file research/foo.md` reads from), so a relative
`bundle_dir` works directly:

```
# build under ./<name>/ in the workspace
bash.shell: mkdir -p roads-by-region/scripts roads-by-region/references
bash.write_file: roads-by-region/SKILL.md            <manifest+body>
bash.write_file: roads-by-region/scripts/report.py   <script>
bash.write_file: roads-by-region/references/howto.md <doc>
```

Then validate + save against that directory:

```
skill:validate(bundle_dir: "roads-by-region")
# fix any reported problems, then register:
skill:save(bundle_dir: "roads-by-region")
```

Keep the bundle directory name distinct from your scratch / research
directories so the three canonical subdirs are unambiguous.
