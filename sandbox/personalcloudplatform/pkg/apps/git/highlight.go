// highlight.go — the syntax-highlighting language whitelist (§16; with
// the vendored highlight.js this supersedes the §5.2 v1 "no syntax
// highlighting" cut). Two maps, one vocabulary: langByExt names a blob
// view's language from its file name, langByFence admits a markdown
// fence info string. Every value is a highlight.js grammar (or alias)
// shipped in assets/vendor/highlightjs — the maps are the ONLY source
// of class attributes, so hostile input can never inject one: an
// unlisted name renders plain.
package git

import "strings"

// langByExt maps a lower-cased file extension (or a whole special file
// name like "dockerfile") to its highlight.js language.
var langByExt = map[string]string{
	".c": "c", ".h": "c",
	".cpp": "cpp", ".cc": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp", ".hxx": "cpp",
	".cs":  "csharp",
	".rb":  "ruby",
	".py":  "python",
	".go":  "go",
	".php": "php",
	".js":  "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascript",
	".ts": "typescript", ".tsx": "typescript",
	".java": "java",
	".rs":   "rust",
	".sh":   "bash", ".bash": "bash", ".zsh": "bash",
	".html": "xml", ".htm": "xml", ".xml": "xml", ".svg": "xml", ".xhtml": "xml",
	".css":  "css",
	".json": "json",
	".yaml": "yaml", ".yml": "yaml",
	".md": "markdown", ".markdown": "markdown",
	".sql": "sql",
	".kt":  "kotlin", ".kts": "kotlin",
	".swift": "swift",
	".diff":  "diff", ".patch": "diff",
	".ini": "ini", ".toml": "toml",
	".mk": "makefile",
	// Whole-filename matches (no meaningful extension).
	"dockerfile": "dockerfile",
	"makefile":   "makefile", "gnumakefile": "makefile",
}

// langByFence maps a markdown fence info string (its first word,
// lower-cased) to a highlight.js language. Aliases cover what people
// actually type; anything absent stays a plain code block.
var langByFence = map[string]string{
	"c": "c", "h": "c",
	"cpp": "cpp", "c++": "cpp", "cc": "cpp",
	"csharp": "csharp", "cs": "csharp", "c#": "csharp",
	"ruby": "ruby", "rb": "ruby",
	"python": "python", "py": "python", "python3": "python",
	"go": "go", "golang": "go",
	"php":        "php",
	"javascript": "javascript", "js": "javascript", "jsx": "javascript",
	"typescript": "typescript", "ts": "typescript", "tsx": "typescript",
	"java": "java",
	"rust": "rust", "rs": "rust",
	"bash": "bash", "sh": "bash", "shell": "shell", "zsh": "bash", "console": "shell",
	"xml": "xml", "html": "xml", "svg": "xml",
	"css":  "css",
	"json": "json",
	"yaml": "yaml", "yml": "yaml",
	"markdown": "markdown", "md": "markdown",
	"sql":    "sql",
	"kotlin": "kotlin", "kt": "kotlin",
	"swift": "swift",
	"diff":  "diff", "patch": "diff",
	"dockerfile": "dockerfile", "docker": "dockerfile",
	"ini": "ini", "toml": "toml",
	"makefile": "makefile", "make": "makefile",
}

// blobLang names the highlight language for a repo file path ("" =
// plain). The match is the last path segment's extension, falling back
// to the whole (extensionless) file name for Dockerfile/Makefile kin.
func blobLang(path string) string {
	name := strings.ToLower(path[strings.LastIndex(path, "/")+1:])
	if i := strings.LastIndex(name, "."); i > 0 {
		if lang, ok := langByExt[name[i:]]; ok {
			return lang
		}
		return ""
	}
	return langByExt[name]
}

// fenceLang whitelists a fence info string ("go", "c++ some-note", …)
// into a language class value ("" = no class). ONLY the returned map
// value — never the input — reaches HTML, so the class attribute is
// injection-proof by construction.
func fenceLang(info string) string {
	first := strings.ToLower(strings.TrimSpace(info))
	if i := strings.IndexAny(first, " \t"); i >= 0 {
		first = first[:i]
	}
	return langByFence[first]
}
