package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits applied to every synced repository. They bound disk, memory and the
// prompt budget a hostile or careless repo can consume.
const (
	maxFileBytes     = 256 << 10
	maxFilesPerSkill = 64
	maxSkillBytes    = 2 << 20
	maxSkills        = 100
	maxDescription   = 1024
	maxPathBytes     = 1024

	marketplacePath = ".claude-plugin/marketplace.json"
)

var (
	skillDirRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	pluginNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// treeEntry is one line of `git ls-tree -r -l`.
type treeEntry struct {
	Mode string
	Type string
	OID  string
	Size int64
	Path string
}

func (e treeEntry) regular() bool {
	return e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755")
}

// blobReader returns the content of the given blob ids.
type blobReader func(ctx context.Context, oids []string) (map[string][]byte, error)

type skillFile struct {
	Rel  string // path relative to the skill directory
	Data []byte
}

type loadedSkill struct {
	Info  SkillInfo
	Files []skillFile
}

// discovery is the result of scanning one commit.
type discovery struct {
	Skills  []loadedSkill
	Skipped []Skipped
}

// parseTree parses NUL-separated `git ls-tree -r -l -z` output.
func parseTree(out []byte) ([]treeEntry, error) {
	var entries []treeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			return nil, fmt.Errorf("malformed tree entry")
		}
		f := strings.Fields(meta)
		if len(f) != 4 {
			return nil, fmt.Errorf("malformed tree entry")
		}
		size := int64(-1)
		if f[3] != "-" {
			n, err := strconv.ParseInt(f[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("malformed tree entry size")
			}
			size = n
		}
		entries = append(entries, treeEntry{Mode: f[0], Type: f[1], OID: f[2], Size: size, Path: p})
	}
	return entries, nil
}

// cleanRelativeSource validates a marketplace plugin "source" that must stay
// inside the repository. It returns the cleaned slash path ("" is the root).
func cleanRelativeSource(src string) (string, error) {
	if src == "" {
		return "", fmt.Errorf("empty source")
	}
	if strings.ContainsAny(src, "\\\x00") || strings.HasPrefix(src, "/") {
		return "", fmt.Errorf("source must be a relative path")
	}
	for _, seg := range strings.Split(src, "/") {
		if seg == ".." {
			return "", fmt.Errorf("source must not contain '..'")
		}
	}
	c := path.Clean(src)
	if c == "." {
		return "", nil
	}
	return c, nil
}

// validPathSegments rejects empty, dot and control-character path segments.
func validPathSegments(p string) bool {
	if p == "" || len(p) > maxPathBytes || !utf8.ValidString(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || len(seg) > 255 {
			return false
		}
		if strings.ContainsFunc(seg, unicode.IsControl) || strings.Contains(seg, "\\") {
			return false
		}
	}
	return true
}

type marketplace struct {
	Plugins []struct {
		Name   string          `json:"name"`
		Source json.RawMessage `json:"source"`
	} `json:"plugins"`
}

type pluginRoot struct {
	name string
	base string // "" for the repo root, else a clean slash path
}

// discover scans entries for skills following the marketplace layout (with a
// fallback to a root skills/ directory when there is no marketplace file) and
// returns the validated skills in deterministic order.
func discover(ctx context.Context, entries []treeEntry, read blobReader) (discovery, error) {
	byPath := make(map[string]treeEntry, len(entries))
	for _, e := range entries {
		byPath[e.Path] = e
	}
	var d discovery

	roots, useRoot := marketplaceRoots(ctx, byPath, read, &d)
	if useRoot {
		roots = append(roots, pluginRoot{})
	}

	seen := map[string]string{} // skill name -> "plugin/path" that owns it
	for _, root := range roots {
		prefix := "skills/"
		if root.base != "" {
			prefix = root.base + "/skills/"
		}
		groups := groupSkillDirs(entries, prefix)
		names := make([]string, 0, len(groups))
		for n := range groups {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, dirName := range names {
			dirPath := prefix + dirName
			skill, reason, err := loadSkill(ctx, dirName, dirPath, root.name, groups[dirName], byPath, read)
			if err != nil {
				return d, err
			}
			switch {
			case reason != "":
				d.Skipped = append(d.Skipped, Skipped{Path: dirPath, Reason: reason})
			case seen[skill.Info.Name] != "":
				d.Skipped = append(d.Skipped, Skipped{Path: dirPath, Reason: fmt.Sprintf("duplicate skill name %q (already provided by %s)", skill.Info.Name, seen[skill.Info.Name])})
			case len(d.Skills) >= maxSkills:
				d.Skipped = append(d.Skipped, Skipped{Path: dirPath, Reason: fmt.Sprintf("too many skills (limit %d)", maxSkills)})
			default:
				seen[skill.Info.Name] = dirPath
				d.Skills = append(d.Skills, skill)
			}
		}
	}
	sort.SliceStable(d.Skills, func(i, j int) bool { return d.Skills[i].Info.Name < d.Skills[j].Info.Name })
	return d, nil
}

// marketplaceRoots reads the marketplace file. useRoot is true when the repo
// has no usable marketplace and the root skills/ directory should be scanned.
func marketplaceRoots(ctx context.Context, byPath map[string]treeEntry, read blobReader, d *discovery) (roots []pluginRoot, useRoot bool) {
	e, ok := byPath[marketplacePath]
	if !ok {
		return nil, true
	}
	if !e.regular() || e.Size > maxFileBytes {
		d.Skipped = append(d.Skipped, Skipped{Path: marketplacePath, Reason: "marketplace file is not a regular file under the size limit"})
		return nil, true
	}
	blobs, err := read(ctx, []string{e.OID})
	if err != nil {
		d.Skipped = append(d.Skipped, Skipped{Path: marketplacePath, Reason: "cannot read marketplace file: " + sanitize(err.Error())})
		return nil, true
	}
	var m marketplace
	if err := json.Unmarshal(blobs[e.OID], &m); err != nil {
		d.Skipped = append(d.Skipped, Skipped{Path: marketplacePath, Reason: "malformed marketplace JSON"})
		return nil, true
	}
	for i, p := range m.Plugins {
		label := "plugin:" + sanitize(p.Name)
		if p.Name == "" {
			label = "plugin:#" + strconv.Itoa(i)
		}
		if !pluginNameRe.MatchString(p.Name) {
			d.Skipped = append(d.Skipped, Skipped{Path: label, Reason: "invalid plugin name"})
			continue
		}
		var src string
		if err := json.Unmarshal(p.Source, &src); err != nil {
			d.Skipped = append(d.Skipped, Skipped{Path: label, Reason: "plugin source is not a relative path (remote sources are ignored)"})
			continue
		}
		base, err := cleanRelativeSource(src)
		if err != nil {
			d.Skipped = append(d.Skipped, Skipped{Path: label, Reason: "invalid plugin source: " + err.Error()})
			continue
		}
		roots = append(roots, pluginRoot{name: p.Name, base: base})
	}
	return roots, false
}

// groupSkillDirs groups the entries under prefix by their first path segment.
// Entries directly under prefix (no directory) are kept so a symlinked skill
// directory is reported rather than silently ignored.
func groupSkillDirs(entries []treeEntry, prefix string) map[string][]treeEntry {
	groups := map[string][]treeEntry{}
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Path, prefix)
		if !ok || rest == "" {
			continue
		}
		name, _, nested := strings.Cut(rest, "/")
		if !nested && e.regular() {
			continue // a stray regular file next to the skill directories
		}
		groups[name] = append(groups[name], e)
	}
	return groups
}

// loadSkill validates one skill directory. A non-empty reason means the skill
// is skipped; err is reserved for infrastructure failures.
func loadSkill(ctx context.Context, dirName, dirPath, plugin string, files []treeEntry, byPath map[string]treeEntry, read blobReader) (loadedSkill, string, error) {
	if !skillDirRe.MatchString(dirName) {
		return loadedSkill{}, "invalid skill directory name (allowed: lowercase letters, digits and '-', max 64)", nil
	}
	prefix := dirPath + "/"
	if len(files) > maxFilesPerSkill {
		return loadedSkill{}, fmt.Sprintf("too many files (%d, limit %d)", len(files), maxFilesPerSkill), nil
	}
	var total int64
	for _, e := range files {
		switch {
		case e.Mode == "120000":
			return loadedSkill{}, "contains a symlink: " + sanitize(strings.TrimPrefix(e.Path, prefix)), nil
		case !e.regular():
			return loadedSkill{}, "contains a non-regular entry (submodule or special file): " + sanitize(strings.TrimPrefix(e.Path, prefix)), nil
		case !validPathSegments(strings.TrimPrefix(e.Path, prefix)):
			return loadedSkill{}, "contains an invalid file name", nil
		case e.Size > maxFileBytes:
			return loadedSkill{}, fmt.Sprintf("file too large: %s (%d bytes, limit %d)", sanitize(strings.TrimPrefix(e.Path, prefix)), e.Size, maxFileBytes), nil
		}
		total += e.Size
	}
	if total > maxSkillBytes {
		return loadedSkill{}, fmt.Sprintf("total size %d bytes exceeds limit %d", total, maxSkillBytes), nil
	}
	main, ok := byPath[prefix+"SKILL.md"]
	if !ok || !main.regular() {
		return loadedSkill{}, "missing SKILL.md", nil
	}

	oids := make([]string, 0, len(files))
	for _, e := range files {
		oids = append(oids, e.OID)
	}
	blobs, err := read(ctx, oids)
	if err != nil {
		return loadedSkill{}, "", fmt.Errorf("reading skill %s: %w", dirPath, err)
	}
	fm, ferr := parseFrontmatter(blobs[main.OID])
	if ferr != nil {
		return loadedSkill{}, "invalid SKILL.md frontmatter: " + ferr.Error(), nil
	}
	name, desc := strings.TrimSpace(fm["name"]), strings.TrimSpace(fm["description"])
	switch {
	case name == "":
		return loadedSkill{}, "frontmatter is missing a non-empty name", nil
	case desc == "":
		return loadedSkill{}, "frontmatter is missing a non-empty description", nil
	case name != dirName:
		return loadedSkill{}, fmt.Sprintf("frontmatter name %q does not match directory name %q", sanitize(name), dirName), nil
	case len(desc) > maxDescription:
		return loadedSkill{}, fmt.Sprintf("description longer than %d bytes", maxDescription), nil
	}

	sk := loadedSkill{Info: SkillInfo{Name: name, Plugin: plugin, Description: desc}}
	for _, e := range files {
		data, ok := blobs[e.OID]
		if !ok || int64(len(data)) != e.Size {
			return loadedSkill{}, "", fmt.Errorf("skill %s: blob %s unavailable or size mismatch", dirPath, e.OID)
		}
		sk.Files = append(sk.Files, skillFile{Rel: strings.TrimPrefix(e.Path, prefix), Data: data})
	}
	sort.Slice(sk.Files, func(i, j int) bool { return sk.Files[i].Rel < sk.Files[j].Rel })
	return sk, "", nil
}

// parseFrontmatter reads the YAML-ish header of a SKILL.md: top-level
// "key: value" pairs, quoted scalars, block scalars (> and |) and indented
// continuation lines. Nested structures are not interpreted.
func parseFrontmatter(data []byte) (map[string]string, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("not valid UTF-8")
	}
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return nil, fmt.Errorf("must start with a '---' line")
	}
	out := map[string]string{}
	var key string
	closed := false
	for _, line := range lines[1:] {
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "---" || trimmed == "..." {
			closed = true
			break
		}
		if strings.TrimSpace(trimmed) == "" || strings.HasPrefix(strings.TrimSpace(trimmed), "#") && line[0] != ' ' && line[0] != '\t' {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if key != "" {
				out[key] = strings.TrimSpace(out[key] + " " + strings.TrimSpace(line))
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			key = ""
			continue
		}
		key = k
		v = strings.TrimSpace(v)
		switch v {
		case ">", "|", ">-", "|-", ">+", "|+":
			v = ""
		}
		out[key] = unquoteScalar(v)
	}
	if !closed {
		return nil, fmt.Errorf("missing closing '---' line")
	}
	return out, nil
}

func unquoteScalar(v string) string {
	if len(v) >= 2 {
		switch {
		case v[0] == '"' && v[len(v)-1] == '"':
			if u, err := strconv.Unquote(v); err == nil {
				return u
			}
			return v[1 : len(v)-1]
		case v[0] == '\'' && v[len(v)-1] == '\'':
			return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
		}
	}
	return v
}
