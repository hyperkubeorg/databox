# Ace editor (vendored)

- Project: Ace (Ajax.org Cloud9 Editor) — https://github.com/ajaxorg/ace
  packaged as ace-builds — https://github.com/ajaxorg/ace-builds
- Version: 1.44.0 (pinned; the directory name carries the version)
- License: BSD-3-Clause (LICENSE in this directory, verbatim upstream)
- Vendored: 2026-07-15
- Local modifications: none

## Source and exact download URL

All files were extracted unmodified from the official npm release
tarball (the `src-min-noconflict` build flavor plus the package
LICENSE):

    https://registry.npmjs.org/ace-builds/-/ace-builds-1.44.0.tgz

| File | Tarball path |
|---|---|
| `ace.js` | `package/src-min-noconflict/ace.js` |
| `ext-searchbox.js` | `package/src-min-noconflict/ext-searchbox.js` |
| `mode-*.js` | `package/src-min-noconflict/mode-*.js` |
| `LICENSE` | `package/LICENSE` |

## Included modes

c_cpp (C and C++), csharp, css, diff, dockerfile, golang, html, ini,
java, javascript, json, kotlin, makefile, markdown, php, python, ruby,
rust, sh (bash/shell), sql, swift, toml, typescript, xml, yaml. Plain
text is built into `ace.js`.

Modes are self-contained (each bundles its dependencies) and are loaded
on demand: the editor page sets `ace.config.set('basePath', …)` to this
directory's asset route, so switching the path extension live-switches
the mode with one script fetch.

## Deliberately NOT shipped

- Stock themes: the editor uses our own `ace/theme/pcp`, defined inline
  in `git_repo_edit.tpl` from the PCP design tokens, so it follows both
  the dark and the light theme.
- Worker files: the editor runs with `useWorker: false` — no background
  linting, no worker/loader machinery.

## Adding a mode

Extract `package/src-min-noconflict/mode-<name>.js` from THIS version's
tarball into this directory and extend the extension→mode map in
`git_repo_edit.tpl` (`aceModeByExt`).
