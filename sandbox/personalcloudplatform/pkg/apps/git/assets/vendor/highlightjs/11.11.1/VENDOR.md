# highlight.js (vendored)

- Project: highlight.js — https://github.com/highlightjs/highlight.js
- Version: 11.11.1 (pinned; the directory name carries the version)
- License: BSD-3-Clause (LICENSE in this directory, verbatim upstream)
- Vendored: 2026-07-15
- Local modifications: none

## Files and exact download URLs

| File | Source |
|---|---|
| `highlight.min.js` | https://cdnjs.cloudflare.com/ajax/libs/highlight.js/11.11.1/highlight.min.js |
| `dockerfile.min.js` | https://cdnjs.cloudflare.com/ajax/libs/highlight.js/11.11.1/languages/dockerfile.min.js |
| `LICENSE` | https://raw.githubusercontent.com/highlightjs/highlight.js/11.11.1/LICENSE |

cdnjs mirrors the official upstream release build unmodified;
`highlight.min.js` is the stock "common languages" bundle.

## Included languages

From the common bundle: bash, c, cpp, csharp, css, diff, go, graphql,
ini (alias: toml), java, javascript, json, kotlin, less, lua, makefile,
markdown, objectivec, perl, php, php-template, plaintext, python,
python-repl, r, ruby, rust, scss, shell, sql, swift, typescript, vbnet,
wasm, xml (alias: html), yaml.

Added separately: dockerfile (`dockerfile.min.js`, alias: docker).

## Adding a language

Download `languages/<lang>.min.js` for THIS version from cdnjs, drop it
in this directory, add a `<script>` include next to the dockerfile one
in `git_repo_shell.tpl` ("gitassets"), and extend the whitelist maps in
`pkg/apps/git/highlight.go`.

## Theme

No upstream theme CSS is shipped. Token colors are our own, written
against the PCP design tokens (see the syntax-token section of
`git_styles.tpl`), so highlighting obeys both the dark and light themes.
