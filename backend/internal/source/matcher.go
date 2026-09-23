// 本文件实现多仓库关联匹配打分：把堆栈/日志/入口线索映射为候选仓库与待加载文件线索。
//
// 打分权重与阈值全部集中在 repoWeights 中，便于按业务反馈调参而不影响调用方。
package source

import (
	"regexp"
	"sort"
	"strings"

	"github.com/codeagent/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// 打分权重
// ---------------------------------------------------------------------------

// repoWeights 匹配打分权重（原始分，最终归一化到 0-1）。
type repoWeights struct {
	PathPrefix     float64 // 帧文件路径命中 PathPrefixes
	PackagePrefix  float64 // 帧包名命中 PackagePrefixes
	ClassName      float64 // 类名与仓库 Key/Name 相似
	ArtifactName   float64 // 日志/堆栈包含 ArtifactNames
	HostPattern    float64 // 日志/堆栈包含 HostPatterns
	EndpointMatch  float64 // 日志/堆栈包含 EndpointPatterns
	Keyword        float64 // 兜底关键词
	Service        float64 // 帧 Services 命中仓库 Key
	EntryFile      float64 // EntryFiles 命中 PathPrefixes
	LayerBonus     float64 // 分层加成上限（按层取值，见 layerWeight）
	EntryRepo      float64 // 分组链路入口成员
	UnknownPenalty float64 // 完全无命中的惩罚（不参与归一化分子）
}

// repoWeightsDefault 默认权重。
func repoWeightsDefault() repoWeights {
	return repoWeights{
		PathPrefix:    0.45,
		PackagePrefix: 0.40,
		ClassName:     0.35,
		ArtifactName:  0.30,
		HostPattern:   0.25,
		EndpointMatch: 0.20,
		Keyword:       0.10,
		Service:       0.30,
		EntryFile:     0.30,
		LayerBonus:    0.08,
		EntryRepo:     0.15,
	}
}

const (
	// matchThreshold 进入结果的最小归一化得分。
	//
	// 取 0.15 的意图：单一强信号（路径前缀 0.45 / 包名 0.40 / 类名 0.35 / 产物 0.30）
	// 足以让仓库进入候选；而仅靠兜底关键词（0.10）或分层加成（≤0.08）的仓库会被淘汰。
	matchThreshold = 0.15
	// maxCandidates 结果数量上限。
	maxCandidates = 8
	// maxHintsPerRepo 单仓库最多产出的文件线索数。
	maxHintsPerRepo = 16
	// maxHintsFromFrames 由帧直接采样的文件线索上限。
	maxHintsFromFrames = 10
	// matchNormalizeBase 归一化分母（覆盖强信号权重之和），保证结果落在 0-1。
	matchNormalizeBase = 1.5
)

// ---------------------------------------------------------------------------
// 输入
// ---------------------------------------------------------------------------

// MatchInput 匹配输入。
type MatchInput struct {
	Stack        *domain.StackAnalysis
	GroupRepos   []*domain.Repository // 候选范围（分组模式=分组成员；单仓库模式=唯一仓库）
	EntryRepoIDs []string             // 分组定义的链路入口
	EntryFiles   []string             // 调用方指定嫌疑文件
	Logs         string
}

// Match 返回按得分降序的候选仓库（含需要加载的文件线索）。
//
// 无任何命中时返回空切片，引擎层据此降级为堆栈文本分析。
func Match(in MatchInput) []domain.RepoCandidate {
	if len(in.GroupRepos) == 0 {
		return []domain.RepoCandidate{}
	}
	w := repoWeightsDefault()
	ev := buildEvidence(in)

	candidates := make([]domain.RepoCandidate, 0, len(in.GroupRepos))
	for _, repo := range in.GroupRepos {
		if repo == nil {
			continue
		}
		score, matchedBy := scoreRepo(repo, ev, w)
		if len(matchedBy) == 0 {
			continue
		}
		norm := score / matchNormalizeBase
		if norm > 1 {
			norm = 1
		}
		if norm < matchThreshold {
			continue
		}
		candidates = append(candidates, domain.RepoCandidate{
			RepositoryID: repo.ID,
			RepoKey:      repo.Key,
			Name:         repo.Name,
			Layer:        repo.Layer,
			Score:        round4(norm),
			MatchedBy:    matchedBy,
			Hints:        buildHints(repo, ev),
		})
	}
	if len(candidates) == 0 {
		return []domain.RepoCandidate{}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		li, lj := layerRank(candidates[i].Layer), layerRank(candidates[j].Layer)
		if li != lj {
			return li < lj
		}
		return candidates[i].RepoKey < candidates[j].RepoKey
	})
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}
	return candidates
}

// ---------------------------------------------------------------------------
// 证据收集
// ---------------------------------------------------------------------------

// repoEvidence 一次匹配所需的全部文本证据（预计算，避免逐仓库重复扫描）。
type repoEvidence struct {
	stack        *domain.StackAnalysis
	files        []string // 帧文件路径（保持顺序，已归一化）
	logFiles     []string // 日志/堆栈文本中出现的源码路径
	fileBases    []string // 帧文件 basename
	packages     []string // 帧包名（保序去重）
	services     []string // 帧内服务名
	stackText    string   // 堆栈原始文本
	hayText      string   // 堆栈 + 日志的小写文本（用于关键词/工件/主机匹配）
	logText      string   // 日志原始文本
	entryFiles   []string // 调用方指定嫌疑文件
	entrySet     map[string]bool
	entryRepoIDs map[string]bool // 分组链路入口仓库 ID 集合
}

// logPathRe 从任意文本中提取源码文件路径（用于日志中的路径线索）。
var logPathRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./@-]*\.(?:java|kt|go|py|ts|tsx|js|jsx|vue|php|cs|rs|rb|sql|yaml|yml|xml|json)`)

// maxLogFilePaths 单次匹配从文本中提取的日志路径上限。
const maxLogFilePaths = 64

// buildEvidence 预计算匹配证据。
func buildEvidence(in MatchInput) *repoEvidence {
	ev := &repoEvidence{
		stack:        in.Stack,
		entrySet:     map[string]bool{},
		entryRepoIDs: map[string]bool{},
		logText:      in.Logs,
	}
	for _, id := range in.EntryRepoIDs {
		if strings.TrimSpace(id) != "" {
			ev.entryRepoIDs[id] = true
		}
	}
	var b strings.Builder
	if in.Stack != nil {
		for _, f := range in.Stack.Frames {
			if p := normalizeRepoPath(f.File); p != "" {
				ev.files = append(ev.files, p)
				ev.fileBases = append(ev.fileBases, baseName(p))
			}
			if f.Package != "" {
				ev.packages = appendUniqueFold(ev.packages, f.Package)
			}
		}
		ev.services = append(ev.services, in.Stack.Services...)
		b.WriteString(stackText(in.Stack))
	}
	b.WriteString("\n")
	b.WriteString(in.Logs)
	ev.stackText = b.String()
	ev.hayText = strings.ToLower(ev.stackText)
	// 从堆栈+日志文本中提取源码路径线索（去重保序）。
	seenPath := map[string]bool{}
	for _, m := range logPathRe.FindAllString(ev.stackText, maxLogFilePaths*2) {
		p := normalizeRepoPath(m)
		if p == "" || seenPath[p] || len(ev.logFiles) >= maxLogFilePaths {
			continue
		}
		seenPath[p] = true
		ev.logFiles = append(ev.logFiles, p)
	}
	for _, f := range in.EntryFiles {
		p := normalizeRepoPath(f)
		if p == "" {
			continue
		}
		if !ev.entrySet[p] {
			ev.entrySet[p] = true
			ev.entryFiles = append(ev.entryFiles, p)
		}
	}
	return ev
}

// ---------------------------------------------------------------------------
// 单仓库打分
// ---------------------------------------------------------------------------

// scoreRepo 计算单个仓库的原始得分与命中原因（中文）。
func scoreRepo(repo *domain.Repository, ev *repoEvidence, w repoWeights) (float64, []string) {
	score := 0.0
	matched := make([]string, 0, 6)
	rules := repo.MatchRules
	keyNorm := normKey(repo.Key)
	nameNorm := normKey(repo.Name)

	// 1) 帧文件路径命中 PathPrefixes
	if terms := pathTerms(rules.PathPrefixes); len(terms) > 0 {
		if hit, term, file := firstPathHit(ev.files, terms); hit {
			score += w.PathPrefix
			matched = append(matched, "堆栈文件路径命中 "+term+"（"+file+"）")
		} else if hit, term, file := firstPathHit(ev.logFiles, terms); hit {
			// 日志中出现的源码路径同样是强线索（例如网关打印的下游文件路径）。
			score += w.PathPrefix
			matched = append(matched, "日志文件路径命中 "+term+"（"+file+"）")
		}
	}

	// 2) 帧包名命中 PackagePrefixes
	if terms := pkgTerms(rules.PackagePrefixes); len(terms) > 0 {
		if hit, term, pkg := firstPackageHit(ev.packages, terms); hit {
			score += w.PackagePrefix
			matched = append(matched, "堆栈包名命中 "+term+"（"+pkg+"）")
		}
	}

	// 3) 类名与仓库 Key/Name 相似
	if len(ev.fileBases) > 0 && keyNorm != "" {
		for _, base := range ev.fileBases {
			if classMatchesRepo(base, keyNorm, nameNorm) {
				score += w.ClassName
				matched = append(matched, "堆栈类名 "+base+" 与仓库标识匹配")
				break
			}
		}
	}

	// 4) 日志/堆栈包含 ArtifactNames
	if hit, term := haystackHit(ev.hayText, rules.ArtifactNames); hit {
		score += w.ArtifactName
		matched = append(matched, "日志/堆栈包含构建产物 "+term)
	}

	// 5) 日志/堆栈包含 HostPatterns
	if hit, term := haystackHit(ev.hayText, rules.HostPatterns); hit {
		score += w.HostPattern
		matched = append(matched, "日志/堆栈包含主机标识 "+term)
	}

	// 6) 日志/堆栈包含 EndpointPatterns（支持 ** 通配）
	if hit, term, ep := firstEndpointHit(ev.hayText, rules.EndpointPatterns); hit {
		score += w.EndpointMatch
		matched = append(matched, "接口路径命中 "+term+"（"+ep+"）")
	}

	// 7) 兜底关键词
	if hit, term := haystackHit(ev.hayText, rules.Keywords); hit {
		score += w.Keyword
		matched = append(matched, "命中关键词 "+term)
	}

	// 8) 帧内服务名命中仓库 Key
	if svc := firstServiceHit(ev.services, keyNorm); svc != "" {
		score += w.Service
		matched = append(matched, "日志/堆栈服务名 "+svc+" 与仓库标识匹配")
	}

	// 9) EntryFiles 命中 PathPrefixes
	if len(ev.entryFiles) > 0 {
		if hit, term, file := firstPathHit(ev.entryFiles, pathTerms(rules.PathPrefixes)); hit {
			score += w.EntryFile
			matched = append(matched, "调用方指定嫌疑文件 "+file+" 命中 "+term)
		}
	}

	// 10) Layer 加成（仅用于链路排序，不压过直接命中）
	if bonus := layerWeight(repo.Layer, w); bonus > 0 {
		score += bonus
		matched = append(matched, "分层加成 "+string(repo.Layer))
	}

	// 11) 分组链路入口成员
	if ev.hasEntryRepo(repo.ID) {
		score += w.EntryRepo
		matched = append(matched, "分组链路入口仓库")
	}
	return score, matched
}

// hasEntryRepo 判断仓库是否属于分组入口（延迟注入到证据中）。
func (ev *repoEvidence) hasEntryRepo(repoID string) bool {
	return ev.entryRepoIDs[repoID]
}

// ---------------------------------------------------------------------------
// 归一化与匹配原语
// ---------------------------------------------------------------------------

// normKey 归一化仓库标识 / 类名：去扩展名、小写、把分隔符「-_./\」折叠掉。
//
// 使得 order-service ↔ OrderService.java ↔ order_service ↔ orderservice 等价。
func normKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\\", "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	for _, ext := range []string{".java", ".php", ".py", ".ts", ".tsx", ".js", ".jsx", ".go", ".cs", ".rs", ".rb", ".kt"} {
		if strings.HasSuffix(strings.ToLower(s), ext) {
			s = s[:len(s)-len(ext)]
			break
		}
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case '-', '_', '.', ' ', '/', '\\', ':':
			continue
		default:
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// pathTerms 归一化路径前缀（统一 /、去掉首尾斜杠）。
func pathTerms(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = normalizeRepoPath(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// pkgTerms 归一化包前缀：同时产出 `com.acme.order`（小写）与 `com/acme/order`（斜杠形态）。
func pkgTerms(in []string) []string {
	out := make([]string, 0, len(in)*2)
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.ToLower(strings.TrimSpace(v))
		v = strings.TrimPrefix(v, "package ")
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		norm := normalizeRepoPath(strings.ReplaceAll(p, ".", "/"))
		norm = strings.ReplaceAll(norm, "\\", "/")
		if norm == "" {
			continue
		}
		add(strings.ReplaceAll(norm, "/", "."))
		add(norm)
	}
	return out
}

// firstPathHit 在候选路径中做前缀匹配，返回命中的归一化前缀与样例路径。
func firstPathHit(paths, prefixes []string) (bool, string, string) {
	for _, p := range paths {
		pl := strings.ToLower(p)
		for _, prefix := range prefixes {
			prefix = strings.Trim(prefix, "/")
			if prefix == "" {
				continue
			}
			pre := strings.ToLower(prefix)
			if pl == pre || strings.HasPrefix(pl, pre+"/") || strings.Contains(pl, "/"+pre+"/") {
				return true, prefix, p
			}
		}
	}
	return false, "", ""
}

// firstPackageHit 在帧包名中做前缀匹配（大小写与 `/`↔`.` 归一化）。
func firstPackageHit(packages, prefixes []string) (bool, string, string) {
	for _, pkg := range packages {
		pn := strings.ToLower(strings.ReplaceAll(pkg, "/", "."))
		for _, prefix := range prefixes {
			pre := strings.ToLower(strings.ReplaceAll(strings.Trim(prefix, "."), "/", "."))
			if pre == "" {
				continue
			}
			if pn == pre || strings.HasPrefix(pn, pre+".") {
				return true, prefix, pkg
			}
		}
	}
	return false, "", ""
}

// classMatchesRepo 判断堆栈类名与仓库标识是否指向同一业务实体。
func classMatchesRepo(base, keyNorm, nameNorm string) bool {
	b := normKey(base)
	if b == "" {
		return false
	}
	if b == keyNorm || (nameNorm != "" && b == nameNorm) {
		return true
	}
	if len(b) >= 5 {
		if keyNorm != "" && strings.Contains(keyNorm, b) {
			return true
		}
		if nameNorm != "" && strings.Contains(nameNorm, b) {
			return true
		}
	}
	if len(keyNorm) >= 5 && strings.Contains(b, keyNorm) {
		return true
	}
	if len(nameNorm) >= 5 && strings.Contains(b, nameNorm) {
		return true
	}
	return false
}

// haystackHit 在（小写）文本中查找任意词项，返回命中的原始词项。
func haystackHit(hay string, terms []string) (bool, string) {
	if hay == "" {
		return false, ""
	}
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if strings.Contains(hay, strings.ToLower(t)) {
			return true, t
		}
	}
	return false, ""
}

// firstEndpointHit 在文本中做接口路径匹配（支持 ** 通配）。
func firstEndpointHit(hay string, patterns []string) (bool, string, string) {
	if hay == "" {
		return false, "", ""
	}
	// 先收集文本中出现过的路径候选，避免对全文做昂贵的通配匹配。
	candidates := endpointPathRe.FindAllString(hay, 64)
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if !strings.Contains(pattern, "*") && !strings.Contains(pattern, "?") {
			p := strings.ToLower(pattern)
			if strings.Contains(hay, p) {
				return true, pattern, p
			}
			continue
		}
		re := globMatcher(pattern)
		lp := strings.ToLower(pattern)
		for _, c := range candidates {
			if re(c) || strings.Contains(c, lp) {
				return true, pattern, c
			}
		}
	}
	return false, "", ""
}

// firstServiceHit 在服务名列表中查找与仓库标识归一化后一致的项。
func firstServiceHit(services []string, keyNorm string) string {
	if keyNorm == "" {
		return ""
	}
	for _, s := range services {
		sn := normKey(s)
		if sn == "" {
			continue
		}
		if sn == keyNorm || strings.HasPrefix(sn, keyNorm) || strings.HasPrefix(keyNorm, sn) {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 通配匹配
// ---------------------------------------------------------------------------

// globMatcher 返回 `**` 通配匹配函数（大小写不敏感，直接基于 glob 语义）。
func globMatcher(pattern string) func(string) bool {
	p := strings.ToLower(pattern)
	return func(s string) bool {
		return globSimpleMatch(p, strings.ToLower(s))
	}
}

// globSimpleMatch 通用 glob 匹配：`**` 匹配任意字符（可跨 `/`），`*` 匹配非 `/` 字符，
// `?` 匹配单个非 `/` 字符。
func globSimpleMatch(pattern, s string) bool {
	// 动态规划实现，避免递归回溯导致的指数复杂度。
	p := []rune(pattern)
	t := []rune(s)
	n, m := len(p), len(t)
	dp := make([][]bool, n+1)
	for i := range dp {
		dp[i] = make([]bool, m+1)
	}
	dp[0][0] = true
	for i := 1; i <= n; i++ {
		if p[i-1] == '*' {
			doubleStar := i >= 2 && p[i-2] == '*'
			if doubleStar {
				dp[i][0] = dp[i-1][0] || dp[i-2][0]
			} else {
				dp[i][0] = dp[i-1][0]
			}
		}
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			switch p[i-1] {
			case '*':
				doubleStar := i >= 2 && p[i-2] == '*'
				if doubleStar {
					dp[i][j] = dp[i-1][j] || dp[i-2][j] || dp[i][j-1]
				} else {
					dp[i][j] = dp[i-1][j] || (dp[i][j-1] && t[j-1] != '/')
				}
			case '?':
				dp[i][j] = dp[i-1][j-1] && t[j-1] != '/'
			default:
				dp[i][j] = dp[i-1][j-1] && p[i-1] == t[j-1]
			}
		}
	}
	return dp[n][m]
}

// ---------------------------------------------------------------------------
// 分层
// ---------------------------------------------------------------------------

// layerWeight 返回分层加成（gateway 0.08 / frontend 0.06 / service 0.04 / middleware 0.02）。
func layerWeight(layer domain.RepoLayer, w repoWeights) float64 {
	switch layer {
	case domain.LayerGateway:
		return w.LayerBonus
	case domain.LayerFrontend:
		return w.LayerBonus * 0.75
	case domain.LayerService:
		return w.LayerBonus * 0.5
	case domain.LayerMiddleware:
		return w.LayerBonus * 0.25
	default:
		return 0
	}
}

// layerRank 返回分层排序序位（越小越靠链路入口）。
func layerRank(layer domain.RepoLayer) int {
	switch layer {
	case domain.LayerGateway:
		return 0
	case domain.LayerFrontend:
		return 1
	case domain.LayerService:
		return 2
	case domain.LayerMiddleware:
		return 3
	case domain.LayerLibrary:
		return 4
	default:
		return 5
	}
}

// ---------------------------------------------------------------------------
// 文件线索生成
// ---------------------------------------------------------------------------

// buildHints 生成该仓库需要优先加载的文件线索（同路径去重，按优先级降序）。
func buildHints(repo *domain.Repository, ev *repoEvidence) []domain.FileHint {
	best := map[string]domain.FileHint{}
	add := func(h domain.FileHint) {
		h.Path = normalizeRepoPath(h.Path)
		if h.Path == "" {
			return
		}
		if old, ok := best[h.Path]; ok {
			// 同路径保留更高优先级者，并保留行号信息。
			if h.Priority > old.Priority {
				if h.Line == 0 {
					h.Line = old.Line
				}
				best[h.Path] = h
			} else if old.Line == 0 && h.Line > 0 {
				old.Line = h.Line
				best[h.Path] = old
			}
			return
		}
		best[h.Path] = h
	}

	// 1) 栈帧直接给出的文件与类名（最高优先级）
	n := 0
	for i, f := range ev.files {
		if n >= maxHintsFromFrames {
			break
		}
		add(domain.FileHint{Path: f, Reason: "frame", Line: frameLineAt(ev, i), Priority: 1.0})
		n++
	}
	if ev.stack != nil {
		for i, f := range ev.stack.Frames {
			if i >= maxHintsFromFrames {
				break
			}
			switch f.Language {
			case "java":
				if f.Class != "" {
					add(domain.FileHint{Path: f.Class + ".java", Reason: "symbol", Line: f.Line, Priority: 0.9})
					if f.Package != "" {
						add(domain.FileHint{
							Path:     strings.ReplaceAll(f.Package, ".", "/") + "/" + f.Class + ".java",
							Reason:   "package_prefix",
							Line:     f.Line,
							Priority: 0.85,
						})
					}
				}
			case "php":
				if f.Class != "" {
					add(domain.FileHint{Path: f.Class + ".php", Reason: "symbol", Line: f.Line, Priority: 0.9})
				}
			}
			if f.Class != "" {
				add(domain.FileHint{Path: f.Class, Reason: "symbol", Line: f.Line, Priority: 0.6})
			}
		}
	}

	// 2) 调用方指定嫌疑文件（仅在其命中本仓库路径前缀时才归属该仓库）
	for _, ef := range ev.entryFiles {
		if pathBelongsToRepo(ef, repo) {
			add(domain.FileHint{Path: ef, Reason: "entry", Priority: 0.95})
		}
	}

	// 3) Artifact / Endpoint / Keyword 命中：无帧线索时给出依赖清单等承载文件
	if len(best) == 0 {
		for _, term := range repo.MatchRules.ArtifactNames {
			if strings.Contains(ev.hayText, strings.ToLower(strings.TrimSpace(term))) {
				add(domain.FileHint{Path: dependencyManifest(repo, term), Reason: "artifact", Priority: 0.6})
			}
		}
		for _, term := range repo.MatchRules.Keywords {
			term = strings.TrimSpace(term)
			if term != "" && strings.Contains(ev.hayText, strings.ToLower(term)) {
				add(domain.FileHint{Path: defaultEntryFile(repo), Reason: "symbol", Priority: 0.4})
				break
			}
		}
	}

	out := make([]domain.FileHint, 0, len(best))
	for _, h := range best {
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > maxHintsPerRepo {
		out = out[:maxHintsPerRepo]
	}
	return out
}

// frameLineAt 返回第 i 个帧的行号（帧与文件切片一一对应）。
func frameLineAt(ev *repoEvidence, i int) int {
	if ev.stack == nil || i >= len(ev.stack.Frames) {
		return 0
	}
	return ev.stack.Frames[i].Line
}

// pathBelongsToRepo 判断路径是否落在仓库的路径前缀内。
func pathBelongsToRepo(path string, repo *domain.Repository) bool {
	terms := pathTerms(repo.MatchRules.PathPrefixes)
	if len(terms) == 0 {
		return false
	}
	hit, _, _ := firstPathHit([]string{path}, terms)
	return hit
}

// dependencyManifest 依据语言推断依赖清单文件名。
func dependencyManifest(repo *domain.Repository, artifact string) string {
	lower := strings.ToLower(artifact)
	switch {
	case strings.HasSuffix(lower, ".jar"), strings.HasSuffix(lower, ".war"):
		return "pom.xml"
	case strings.HasSuffix(lower, ".mod"):
		return "go.mod"
	case strings.HasSuffix(lower, ".whl"), strings.HasSuffix(lower, ".txt"):
		return "requirements.txt"
	}
	switch GuessLanguageFamily(repo.Language) {
	case "jvm":
		return "pom.xml"
	case "golang":
		return "go.mod"
	case "python":
		return "requirements.txt"
	case "node":
		return "package.json"
	case "php":
		return "composer.json"
	}
	return "package.json"
}

// defaultEntryFile 依据语言给出常见入口文件。
func defaultEntryFile(repo *domain.Repository) string {
	switch GuessLanguageFamily(repo.Language) {
	case "jvm":
		return "src/main/java"
	case "golang":
		return "main.go"
	case "python":
		return "app.py"
	case "node":
		return "src/index.ts"
	}
	return "src"
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// appendUniqueFold 大小写不敏感去重追加。
func appendUniqueFold(list []string, v string) []string {
	for _, e := range list {
		if strings.EqualFold(e, v) {
			return list
		}
	}
	return append(list, v)
}

// round4 四舍五入到 4 位小数，保证得分输出稳定可断言。
func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}
