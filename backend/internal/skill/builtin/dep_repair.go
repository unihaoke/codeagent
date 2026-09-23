package builtin

// 本文件实现 dependency_repair 技能：解析依赖清单 + 报错信息，定位缺失/冲突依赖并产出清单级补丁。

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/codeagent/backend/internal/domain"
)

// 依赖解析正则。
var (
	rePomDependencyBlock = regexp.MustCompile(`(?s)<dependency>(.*?)</dependency>`)
	rePomGroupID         = regexp.MustCompile(`<groupId>\s*([^<\s]+)\s*</groupId>`)
	rePomArtifactID      = regexp.MustCompile(`<artifactId>\s*([^<\s]+)\s*</artifactId>`)
	rePomVersion         = regexp.MustCompile(`<version>\s*([^<\s]+)\s*</version>`)
	reReqLine            = regexp.MustCompile(`^\s*([A-Za-z0-9_\.\-]+)\s*(?:[=><!~]{1,2}\s*([A-Za-z0-9_\.\*\+]+))?`)
	reGoModRequireItem   = regexp.MustCompile(`^\s*([\w\.\-/]+\.[\w\.\-/]+)\s+(v[\w\.\-+]+)`)
	reGradleDep          = regexp.MustCompile(`(?:implementation|api|compile|testImplementation)\s*[\('"]+([\w\.\-]+):([\w\.\-]+):([\w\.\-]+)`)
	reJSONDepLine        = regexp.MustCompile(`^\s*"([^"]+)"\s*:\s*"([^"]+)"\s*,?\s*$`)
	reVersionConflict    = regexp.MustCompile(`(?i)version conflict[^\d"A-Za-z]*["']?([\w\-\./@]+)["']?[^\d]*(\d+\.\d+(?:\.\d+)?)[^\d]+(\d+\.\d+(?:\.\d+)?)`)
	reOmittedConflict    = regexp.MustCompile(`omitted for conflict with\s+([\d\.]+)`)
	reAnyVersion         = regexp.MustCompile(`\b(\d+\.\d+(?:\.\d+)?)\b`)
)

// depEntry 依赖清单中的一条依赖坐标。
type depEntry struct {
	Name    string
	Group   string
	Version string
	Scope   string
	File    string
	Line    int
}

// canonical 返回依赖的唯一标识（用于比对）。
func (d depEntry) canonical() string {
	if d.Group != "" {
		return strings.ToLower(d.Group + ":" + d.Name)
	}
	return strings.ToLower(d.Name)
}

// handleDependencyRepair dependency_repair 技能处理器。
//
// 输入：dependencyFiles（代码切片数组）、errorMessage（字符串）。
// 输出：findings、actions、patches。
func handleDependencyRepair(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files := decodeSlices(in["dependencyFiles"])
	// 兼容调用方把切片放在 slices 字段（兜底/复用场景）。
	if len(files) == 0 {
		files = decodeSlices(in["slices"])
	}
	errorMessage := stringValue(in["errorMessage"])
	if errorMessage == "" {
		errorMessage = stringValue(in["error"])
	}

	entries := []depEntry{}
	for _, slice := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries = append(entries, parseManifestDependencies(slice)...)
	}
	// 排序保证输出确定性（package.json 等基于 map 解析的结果顺序不稳定）。
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].File != entries[j].File {
			return entries[i].File < entries[j].File
		}
		if entries[i].Line != entries[j].Line {
			return entries[i].Line < entries[j].Line
		}
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Scope < entries[j].Scope
	})
	declared := map[string]depEntry{}
	for _, e := range entries {
		if _, ok := declared[e.canonical()]; !ok {
			declared[e.canonical()] = e
		}
	}

	findings := []map[string]any{}
	actions := []string{}
	type pendingPatch struct {
		slice domain.CodeSlice
		pkg   string
	}
	pendings := []pendingPatch{}

	// ① 缺失依赖：报错信息中出现的包但清单里没有声明。
	missing := extractMissingPackages(errorMessage)
	for _, pkg := range missing {
		group, artifact := splitJavaCoordinate(pkg)
		name := orDefault(artifact, npmName(pkg))
		key := strings.ToLower(orDefault(group+":"+name, pkg))
		if _, ok := declared[key]; ok {
			continue
		}
		if _, ok := declared[strings.ToLower(pkg)]; ok {
			continue
		}
		version := suggestedVersionFromMessage(errorMessage)
		findings = append(findings, map[string]any{
			"kind":             "missing",
			"name":             pkg,
			"currentVersion":   "",
			"suggestedVersion": version,
			"file":             firstManifestPath(files),
			"line":             0,
			"evidence":         "报错信息出现缺失依赖 " + pkg + "，但依赖清单中未声明该坐标",
		})
		actions = append(actions, "补充依赖 "+pkg+"：确认目标版本后在清单文件中添加声明，并重新执行依赖解析（离线环境请预先同步私服/镜像仓库）")
		if len(files) > 0 {
			pendings = append(pendings, pendingPatch{slice: files[0], pkg: pkg})
		}
	}

	// ② 版本冲突：报错信息中的 conflict 描述 + 清单实际版本。
	for _, conflict := range extractVersionConflicts(errorMessage) {
		name, want := conflict[0], conflict[1]
		current := ""
		file := ""
		line := 0
		for _, e := range entries {
			if strings.EqualFold(e.Name, name) || strings.EqualFold(e.Group+":"+e.Name, name) || strings.EqualFold(e.canonical(), strings.ToLower(name)) {
				current, file, line = e.Version, e.File, e.Line
				break
			}
		}
		if current == "" {
			continue
		}
		suggested := want
		if v := highestVersion([]string{current, want}); v != "" {
			suggested = v
		}
		findings = append(findings, map[string]any{
			"kind":             "version_conflict",
			"name":             name,
			"currentVersion":   current,
			"suggestedVersion": suggested,
			"file":             file,
			"line":             line,
			"evidence":         "报错信息提示版本冲突（当前 " + current + "，期望 " + want + "），建议统一到 " + suggested,
		})
		actions = append(actions, "统一依赖 "+name+" 的版本为 "+suggested+"：优先使用依赖管理（dependencyManagement/BOM/resolutions）收敛版本，避免逐个子模块升级")
	}

	// ③ 冗余声明：同一依赖同时出现在 dependencies 与 devDependencies。
	dupSeen := map[string]bool{}
	for _, e := range entries {
		if e.Scope != "devDependencies" {
			continue
		}
		for _, other := range entries {
			if other.Scope != "dependencies" || other.canonical() != e.canonical() {
				continue
			}
			key := e.canonical() + "@dup"
			if dupSeen[key] {
				continue
			}
			dupSeen[key] = true
			findings = append(findings, map[string]any{
				"kind":             "unused",
				"name":             e.Name,
				"currentVersion":   e.Version,
				"suggestedVersion": e.Version,
				"file":             e.File,
				"line":             e.Line,
				"evidence":         "同一依赖同时声明在 dependencies 与 devDependencies，属于冗余声明，建议只保留其一",
			})
			actions = append(actions, "移除冗余声明："+e.Name+" 同时出现在 dependencies 与 devDependencies，按运行时/构建期实际用途保留一处")
		}
	}

	if len(files) == 0 {
		findings = append(findings, map[string]any{
			"kind":             "missing",
			"name":             "",
			"currentVersion":   "",
			"suggestedVersion": "",
			"file":             "",
			"line":             0,
			"evidence":         "未提供任何依赖清单文件（pom.xml / package.json / requirements.txt / go.mod），无法建立依赖坐标基线",
		})
		actions = append(actions, "请在任务上下文中加载依赖清单文件后重试，或人工核对报错中的依赖坐标")
	}

	// ④ 补丁：在清单文件中插入缺失依赖。
	patches := []map[string]any{}
	dropped := 0
	for _, p := range pendings {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lines := splitLines(p.slice.Content)
		group, artifact := splitJavaCoordinate(p.pkg)
		version := suggestedVersionFromMessage(errorMessage)
		oldSnippet, newSnippet, line, rationale, ok := buildDependencyInsertion(lines, p.slice.Path, p.pkg, group, artifact, version)
		if !ok || strings.TrimSpace(oldSnippet) == "" || oldSnippet == newSnippet {
			dropped++
			continue
		}
		if !strings.Contains(p.slice.Content, oldSnippet) {
			dropped++
			continue
		}
		patches = append(patches, map[string]any{
			"repositoryId": p.slice.RepositoryID,
			"repoKey":      p.slice.RepoKey,
			"filePath":     p.slice.Path,
			"action":       string(domain.ActionModify),
			"line":         line,
			"oldSnippet":   oldSnippet,
			"newSnippet":   newSnippet,
			"rationale":    rationale,
			"risk":         string(domain.RiskMedium),
			"confidence":   0.6,
		})
	}
	if dropped > 0 {
		actions = append(actions, "有 "+itoa(dropped)+" 个依赖补丁因清单结构或定位失败被丢弃，请人工确认清单文件内容")
	}
	if len(actions) == 0 {
		actions = append(actions, "未发现明确的依赖问题：清单与报错信息一致，建议进一步核对运行环境与私服可用性")
	}

	sort.SliceStable(findings, func(i, j int) bool {
		return toComparable(findings[i]) < toComparable(findings[j])
	})
	return normalizeOutput(map[string]any{
		"findings": findings,
		"actions":  actions,
		"patches":  patches,
	}), nil
}

// toComparable 为排序生成稳定键。
func toComparable(m map[string]any) string {
	return stringValue(m["kind"]) + "|" + stringValue(m["name"]) + "|" + stringValue(m["file"])
}

// parseManifestDependencies 解析单个清单文件中的依赖条目。
func parseManifestDependencies(slice domain.CodeSlice) []depEntry {
	base := strings.ToLower(baseName(slice.Path))
	lines := splitLines(slice.Content)
	out := []depEntry{}

	switch base {
	case "pom.xml":
		for _, block := range rePomDependencyBlock.FindAllStringSubmatchIndex(slice.Content, -1) {
			body := slice.Content[block[2]:block[3]]
			lineNo := lineOfOffset(slice.Content, block[2])
			entry := depEntry{
				File:    slice.Path,
				Line:    lineNo,
				Scope:   "dependencies",
				Group:   firstSubmatch(rePomGroupID, body),
				Name:    firstSubmatch(rePomArtifactID, body),
				Version: firstSubmatch(rePomVersion, body),
			}
			if entry.Name == "" {
				continue
			}
			out = append(out, entry)
		}
	case "package.json":
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if err := json.Unmarshal([]byte(slice.Content), &pkg); err == nil {
			for name, version := range pkg.Dependencies {
				out = append(out, depEntry{Name: name, Version: version, Scope: "dependencies",
					File: slice.Path, Line: lineOfJSONKey(slice.Content, name)})
			}
			for name, version := range pkg.DevDependencies {
				out = append(out, depEntry{Name: name, Version: version, Scope: "devDependencies",
					File: slice.Path, Line: lineOfJSONKey(slice.Content, name)})
			}
		} else {
			// JSON 不完整时退化为逐行解析。
			scope := ""
			for i, line := range lines {
				if m := rePackageJSONDep.FindStringSubmatch(line); m != nil {
					scope = m[2]
					continue
				}
				if scope == "" {
					continue
				}
				if strings.Contains(line, "}") {
					scope = ""
					continue
				}
				if m := reJSONDepLine.FindStringSubmatch(line); m != nil {
					out = append(out, depEntry{Name: m[1], Version: m[2], Scope: scope, File: slice.Path, Line: i + 1})
				}
			}
		}
	case "requirements.txt":
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
				continue
			}
			if m := reReqLine.FindStringSubmatch(trimmed); m != nil {
				out = append(out, depEntry{Name: m[1], Version: m[2], Scope: "requirements", File: slice.Path, Line: i + 1})
			}
		}
	case "go.mod":
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "module") ||
				strings.HasPrefix(trimmed, "go ") || strings.HasPrefix(trimmed, ")") {
				continue
			}
			trimmed = strings.TrimPrefix(trimmed, "require ")
			if m := reGoModRequireItem.FindStringSubmatch(trimmed); m != nil {
				out = append(out, depEntry{Name: m[1], Version: m[2], Scope: "require", File: slice.Path, Line: i + 1})
			}
		}
	case "build.gradle", "build.gradle.kts":
		for i, line := range lines {
			for _, m := range reGradleDep.FindAllStringSubmatch(line, -1) {
				out = append(out, depEntry{Group: m[1], Name: m[2], Version: m[3], Scope: "implementation",
					File: slice.Path, Line: i + 1})
			}
		}
	default:
		// 未知清单：尝试按 requirements.txt 风格解析。
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if m := reReqLine.FindStringSubmatch(trimmed); m != nil && m[2] != "" {
				out = append(out, depEntry{Name: m[1], Version: m[2], Scope: "unknown", File: slice.Path, Line: i + 1})
			}
		}
	}
	return out
}

// firstSubmatch 返回正则第一个捕获组。
func firstSubmatch(re *regexp.Regexp, text string) string {
	if m := re.FindStringSubmatch(text); m != nil && len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// lineOfOffset 计算字节偏移所在行号（1 起）。
func lineOfOffset(content string, offset int) int {
	if offset <= 0 {
		return 1
	}
	if offset > len(content) {
		offset = len(content)
	}
	return strings.Count(content[:offset], "\n") + 1
}

// lineOfJSONKey 定位 JSON 中某个键所在行号。
func lineOfJSONKey(content, key string) int {
	needle := "\"" + key + "\""
	idx := strings.Index(content, needle)
	if idx < 0 {
		return 0
	}
	return lineOfOffset(content, idx)
}

// extractMissingPackages 从报错信息中提取全部缺失包名。
func extractMissingPackages(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`NoClassDefFoundError:\s*([\w/\.\$]+)`),
		regexp.MustCompile(`ClassNotFoundException:\s*([\w/\.\$]+)`),
		regexp.MustCompile(`Cannot find module '([^']+)'`),
		regexp.MustCompile(`Cannot find module "([^"]+)"`),
		regexp.MustCompile(`No module named '?([\w\.\-]+)'?`),
		regexp.MustCompile(`cannot find package "([^"]+)"`),
		regexp.MustCompile(`no required module provides package ([\w\./\-]+)`),
		regexp.MustCompile(`Unable to find\s+([\w\.\-]+:[\w\.\-]+)`),
	}
	seen := map[string]bool{}
	out := []string{}
	for _, re := range patterns {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			raw := strings.TrimSpace(strings.TrimSuffix(m[1], ".jar"))
			raw = strings.ReplaceAll(raw, "/", ".")
			if idx := strings.Index(raw, "$"); idx > 0 {
				raw = raw[:idx]
			}
			if raw == "" || seen[raw] {
				continue
			}
			seen[raw] = true
			out = append(out, raw)
		}
	}
	sort.Strings(out)
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

// extractVersionConflicts 提取版本冲突信息，返回 [名称, 期望版本] 列表。
func extractVersionConflicts(text string) [][2]string {
	out := [][2]string{}
	if strings.TrimSpace(text) == "" {
		return out
	}
	seen := map[string]bool{}
	for _, m := range reVersionConflict.FindAllStringSubmatch(text, -1) {
		name, want := m[1], m[2]
		if v := highestVersion([]string{m[2], m[3]}); v != "" {
			want = v
		}
		key := name + "@" + want
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, [2]string{name, want})
	}
	if len(out) == 0 {
		if m := reOmittedConflict.FindStringSubmatch(text); m != nil {
			out = append(out, [2]string{"", m[1]})
		}
	}
	return out
}

// suggestedVersionFromMessage 从报错信息中提取首个版本号作为建议版本。
func suggestedVersionFromMessage(text string) string {
	if m := reAnyVersion.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// highestVersion 返回版本号较大者。
func highestVersion(versions []string) string {
	best := ""
	for _, v := range versions {
		v = strings.TrimSpace(strings.TrimPrefix(v, "v"))
		if v == "" {
			continue
		}
		if best == "" || compareVersionNumbers(v, best) > 0 {
			best = v
		}
	}
	return best
}

// compareVersionNumbers 比较形如 1.2.3 的版本号。
func compareVersionNumbers(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(as) {
			av = atoiOr0(as[i])
		}
		if i < len(bs) {
			bv = atoiOr0(bs[i])
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}

// atoiOr0 安全转整数。
func atoiOr0(s string) int {
	n, ok := atoiSafe(s)
	if !ok {
		return 0
	}
	return n
}

// firstManifestPath 返回首个清单文件路径。
func firstManifestPath(files []domain.CodeSlice) string {
	if len(files) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(files))
	for _, f := range files {
		sorted = append(sorted, f.Path)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		if isManifestPath(p) {
			return p
		}
	}
	return sorted[0]
}
