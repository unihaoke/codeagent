package builtin

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/codeagent/backend/internal/domain"
)

// 跨仓库调用证据识别正则。
var (
	reHTTPClient = regexp.MustCompile(`(?i)(resttemplate|webclient|feignclient|openfeign|axios|requests\.(?:get|post|put|delete)|http\.(?:get|post|do)|httpx\.|okhttp|grpc|dubbo|fetch\s*\(|urlopen\s*\()`)
	reURLHost    = regexp.MustCompile(`(?i)https?://([A-Za-z0-9\.\-]+)(?::(\d+))?`)
	reEndpointIn = regexp.MustCompile(`(/[A-Za-z0-9_\-/{}.]*(?:api|v\d)[A-Za-z0-9_\-/{}.]*)`)
	reProtection = regexp.MustCompile(`(?i)(timeout|retry|重试|超时|try\s*\{|catch\s*\(|recover\(\)|if\s*\([^)]*(?:!=\s*null|==\s*null|!=\s*nil|==\s*nil)|Optional|defer\s)`)
)

// layerRank 分层排序权重：越靠近调用入口越小。
func layerRank(layer domain.RepoLayer) int {
	switch layer {
	case domain.LayerFrontend:
		return 0
	case domain.LayerGateway:
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

// handleCrossRepoTrace cross_repo_trace 技能处理器。
//
// 输入：stack、candidates、slices。
// 输出：edges（[]domain.CallEdge 的 map 形态）、chain（有序仓库 Key）、breakPoint、confidence。
func handleCrossRepoTrace(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stack := decodeStack(in["stack"])
	candidates := decodeCandidates(in["candidates"])
	slices := decodeSlices(in["slices"])

	if len(candidates) == 0 {
		return normalizeOutput(map[string]any{
			"edges":      []map[string]any{},
			"chain":      []string{},
			"breakPoint": map[string]any{},
			"confidence": 0.05,
			"reason":     "候选仓库为空，无法推断跨仓库链路",
		}), nil
	}

	ordered := make([]domain.RepoCandidate, len(candidates))
	copy(ordered, candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := layerRank(ordered[i].Layer), layerRank(ordered[j].Layer)
		if ri != rj {
			return ri < rj
		}
		if ordered[i].Score != ordered[j].Score {
			return ordered[i].Score > ordered[j].Score
		}
		return candidateKey(ordered[i]) < candidateKey(ordered[j])
	})

	chain := make([]string, 0, len(ordered))
	for _, c := range ordered {
		key := candidateKey(c)
		if key == "" {
			continue
		}
		if !containsString(chain, key) {
			chain = append(chain, key)
		}
	}

	edges := []domain.CallEdge{}
	// ① 链路骨架：按分层顺序连接相邻候选仓库。
	for i := 0; i+1 < len(ordered); i++ {
		from, to := candidateKey(ordered[i]), candidateKey(ordered[i+1])
		if from == "" || to == "" || from == to {
			continue
		}
		edges = append(edges, domain.CallEdge{
			FromRepo:   from,
			ToRepo:     to,
			Protocol:   "http",
			Evidence:   "依据候选仓库分层（frontend→gateway→service→middleware）与匹配评分推断的链路骨架",
			Confidence: round2(clamp01(0.3 + 0.1*normCandidateScore(ordered[i].Score))),
		})
	}

	// ② 代码证据边：扫描切片中的 HTTP/gRPC 客户端调用与端点字符串。
	codeEdges := 0
	seenEdge := map[string]bool{}
	for _, edge := range edges {
		seenEdge[edge.FromRepo+"->"+edge.ToRepo+"|"+edge.Endpoint] = true
	}
	for _, slice := range slices {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lang := sliceLanguage(slice)
		lines := splitLines(slice.Content)
		fromKey := slice.RepoKey
		if fromKey == "" {
			fromKey = repoKeyByID(candidates, slice.RepositoryID)
		}
		for i, line := range lines {
			if !reHTTPClient.MatchString(line) && !reURLHost.MatchString(line) && !reEndpointIn.MatchString(line) {
				continue
			}
			target, ok := matchTargetInLine(line, candidates, fromKey)
			if !ok {
				continue
			}
			toKey := candidateKey(target)
			endpoint := firstEndpointInLine(line)
			key := fromKey + "->" + toKey + "|" + endpoint
			if seenEdge[key] {
				continue
			}
			seenEdge[key] = true
			symbol := enclosingSymbol(lang, lines, i)
			evidence := "代码证据：" + strings.TrimSpace(line)
			if endpoint != "" {
				evidence += "（端点 " + endpoint + "）"
			}
			edge := domain.CallEdge{
				FromRepo:   fromKey,
				ToRepo:     toKey,
				FromFile:   slice.Path,
				Symbol:     symbol,
				Protocol:   protocolOf(line),
				Endpoint:   endpoint,
				Evidence:   evidence,
				Confidence: 0.7,
			}
			if !reProtection.MatchString(nearbyText(lines, i, 3)) {
				edge.Anomaly = "调用点附近未见超时/重试/空值保护，下游异常会直接向上抛出"
				edge.Confidence = 0.65
			}
			edges = append(edges, edge)
			codeEdges++
		}
	}

	// ③ 断点与异常点。
	breakPoint, frameRepo, frameDesc := resolveBreakPoint(stack, ordered, candidates)

	// 标记与断点相关的链路异常。
	category := ""
	if stack != nil {
		category = stack.Category
	}
	for i := range edges {
		if edges[i].ToRepo == breakPoint.Key && isDownstreamFailure(category) {
			if edges[i].Anomaly == "" {
				edges[i].Anomaly = "下游仓库 " + edges[i].ToRepo + " 报 " + category + "，调用链在该边中断"
			}
		} else if frameRepo != "" && edges[i].FromRepo == frameRepo && edges[i].Anomaly == "" {
			edges[i].Anomaly = "堆栈帧 " + frameDesc + " 命中该仓库，故障在此产生并向上游传播"
		}
	}

	confidence := 0.3
	if codeEdges > 0 {
		confidence += 0.15 * float64(codeEdges)
	}
	if frameRepo != "" {
		confidence += 0.15
	}
	if len(chain) > 1 {
		confidence += 0.05
	}
	confidence = round2(clamp01(confidence))
	if confidence > 0.9 {
		confidence = 0.9
	}

	edgeMaps := make([]map[string]any, 0, len(edges))
	for _, e := range edges {
		edgeMaps = append(edgeMaps, structToMap(e))
	}
	bp := map[string]any{
		"repoKey":      breakPoint.Key,
		"repositoryId": breakPoint.RepositoryID,
		"reason":       breakPoint.Reason,
		"confidence":   breakPoint.Confidence,
	}
	return normalizeOutput(map[string]any{
		"edges":      edgeMaps,
		"chain":      chain,
		"breakPoint": bp,
		"confidence": confidence,
	}), nil
}

// breakPointResult 链路断点推断结果。
type breakPointResult struct {
	Key          string
	RepositoryID string
	Reason       string
	Confidence   float64
}

// resolveBreakPoint 依据堆栈帧与候选评分推断链路断点，同时返回命中的帧所属仓库。
func resolveBreakPoint(stack *domain.StackAnalysis, ordered []domain.RepoCandidate, candidates []domain.RepoCandidate) (breakPointResult, string, string) {
	if stack != nil {
		category := stack.Category
		for _, frame := range stack.Frames {
			repoKey := frame.RepoKey
			repoID := frame.RepositoryID
			if repoKey == "" {
				if hit, ok := matchFrameToCandidates(frame, candidates); ok {
					repoKey, repoID = hit.RepoKey, hit.RepositoryID
				}
			}
			if repoKey == "" {
				continue
			}
			desc := frameDisplay(frame)
			if isDownstreamFailure(category) {
				if downstream, ok := downstreamOf(ordered, repoKey); ok {
					return breakPointResult{
						Key:          downstream,
						RepositoryID: repoIDByKey(candidates, downstream),
						Reason:       "异常类型为 " + category + "，堆栈帧 " + desc + " 位于 " + repoKey + "，故障源头在其下游仓库 " + downstream,
						Confidence:   0.7,
					}, repoKey, desc
				}
			}
			return breakPointResult{
				Key:          repoKey,
				RepositoryID: repoID,
				Reason:       "堆栈帧 " + desc + " 直接命中该仓库，故障在该仓库内部抛出",
				Confidence:   0.65,
			}, repoKey, desc
		}
	}
	// 无可用堆栈帧：按候选评分最高者推断。
	for _, cand := range ordered {
		if candidateKey(cand) == "" {
			continue
		}
		return breakPointResult{
			Key:          candidateKey(cand),
			RepositoryID: cand.RepositoryID,
			Reason:       "未匹配到堆栈帧，按候选仓库匹配评分最高（" + formatScore(normCandidateScore(cand.Score)) + "）推断为断点",
			Confidence:   0.35,
		}, "", ""
	}
	return breakPointResult{Reason: "证据不足，无法定位断点", Confidence: 0.1}, "", ""
}

// isDownstreamFailure 判断异常类型是否指向下游依赖故障。
func isDownstreamFailure(category string) bool {
	switch category {
	case "dependency_missing", "timeout", "connection_refused", "class_not_found":
		return true
	default:
		return false
	}
}

// downstreamOf 返回链路中指定仓库的下一跳。
func downstreamOf(ordered []domain.RepoCandidate, repoKey string) (string, bool) {
	for i, cand := range ordered {
		if candidateKey(cand) != repoKey {
			continue
		}
		if i+1 < len(ordered) {
			next := candidateKey(ordered[i+1])
			if next != "" && next != repoKey {
				return next, true
			}
		}
		return "", false
	}
	return "", false
}

// matchTargetInLine 在代码行中匹配被调用的目标仓库。
func matchTargetInLine(line string, candidates []domain.RepoCandidate, selfKey string) (domain.RepoCandidate, bool) {
	lineNorm := normalizeToken(line)
	if lineNorm == "" {
		return domain.RepoCandidate{}, false
	}
	for _, cand := range candidates {
		key := candidateKey(cand)
		if key == "" || key == selfKey {
			continue
		}
		for _, token := range candidateTokens(cand) {
			if len(token) >= 5 && strings.Contains(lineNorm, token) {
				return cand, true
			}
		}
		// 端点线索：仓库匹配依据里声明的 /api/xxx 出现在代码行中。
		for _, by := range cand.MatchedBy {
			if ep := firstEndpointInLine(by); ep != "" && strings.Contains(line, ep) {
				return cand, true
			}
		}
	}
	return domain.RepoCandidate{}, false
}

// candidateTokens 生成候选仓库的匹配词元（归一化）。
func candidateTokens(cand domain.RepoCandidate) []string {
	out := []string{}
	for _, raw := range append([]string{cand.RepoKey, cand.Name}, cand.MatchedBy...) {
		token := normalizeToken(raw)
		if token == "" {
			continue
		}
		if !containsString(out, token) {
			out = append(out, token)
		}
	}
	for _, hint := range cand.Hints {
		token := normalizeToken(baseName(hint.Path))
		if len(token) >= 6 && !containsString(out, token) {
			out = append(out, token)
		}
	}
	return out
}

// firstEndpointInLine 提取代码行中的端点路径。
func firstEndpointInLine(line string) string {
	if m := reEndpointIn.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	if m := reEndpoint.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	if m := rePathLike.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// protocolOf 判断调用协议。
func protocolOf(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "grpc"), strings.Contains(l, "dubbo"), strings.Contains(l, "feign"), strings.Contains(l, "thrift"):
		return "grpc"
	case strings.Contains(l, "mq"), strings.Contains(l, "kafka"), strings.Contains(l, "rabbit"):
		return "mq"
	default:
		return "http"
	}
}

// nearbyText 拼接附近若干行文本，便于做保护措施检测。
func nearbyText(lines []string, idx, window int) string {
	start := idx - window
	if start < 0 {
		start = 0
	}
	end := idx + window + 1
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n")
}

// enclosingSymbol 向上查找命中行所属的函数/方法名。
func enclosingSymbol(lang string, lines []string, idx int) string {
	for i := idx; i >= 0 && i > idx-40; i-- {
		line := lines[i]
		switch lang {
		case "go":
			if m := reSymGoFunc.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		case "java":
			if m := reSymJavaFunc.FindStringSubmatch(line); m != nil && !isJavaKeyword(m[1]) {
				return m[1]
			}
		case "python":
			if m := reSymPyDef.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		case "node":
			if m := reSymJSFunc.FindStringSubmatch(line); m != nil {
				return m[1]
			}
			if m := reSymJSArrow.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// candidateKey 返回候选仓库的业务键（缺失时退化到 ID）。
func candidateKey(cand domain.RepoCandidate) string {
	if strings.TrimSpace(cand.RepoKey) != "" {
		return cand.RepoKey
	}
	return cand.RepositoryID
}

// repoKeyByID 由仓库 ID 反查业务键。
func repoKeyByID(cands []domain.RepoCandidate, repoID string) string {
	for _, c := range cands {
		if c.RepositoryID == repoID {
			return candidateKey(c)
		}
	}
	return repoID
}

// repoIDByKey 由业务键反查仓库 ID。
func repoIDByKey(cands []domain.RepoCandidate, repoKey string) string {
	for _, c := range cands {
		if candidateKey(c) == repoKey {
			return c.RepositoryID
		}
	}
	return ""
}

// containsString 判断字符串切片是否包含目标值。
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// formatScore 格式化评分，保证输出稳定。
func formatScore(f float64) string {
	return itoa(int(f*100+0.5)) + "%"
}
