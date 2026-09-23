package builtin

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/codeagent/backend/internal/domain"
)

// 归一化辅助正则。
var (
	reNonWord   = regexp.MustCompile(`[^a-z0-9]+`)
	reCamelHead = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

// localizeHit 单条定位命中。
type localizeHit struct {
	RepositoryID string  `json:"repositoryId"`
	RepoKey      string  `json:"repoKey"`
	Path         string  `json:"path"`
	Line         int     `json:"line"`
	Reason       string  `json:"reason"`
	Score        float64 `json:"score"`
	// FrameIndex 命中的堆栈帧序号（0 起），便于前端联动展示。
	FrameIndex int `json:"frameIndex"`
}

// handleErrorLocalize error_localize 技能处理器。
//
// 输入：stack（domain.StackAnalysis 形态）、candidates（[]domain.RepoCandidate 形态）。
// 输出：hits（[{repositoryId, repoKey, path, line, reason, score}]）、confidence。
func handleErrorLocalize(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stack := decodeStack(in["stack"])
	candidates := decodeCandidates(in["candidates"])
	if stack == nil {
		return normalizeOutput(map[string]any{
			"hits":       []map[string]any{},
			"confidence": 0.1,
			"reason":     "缺少 stack 输入，无法进行报错定位",
		}), nil
	}
	if len(candidates) == 0 {
		return normalizeOutput(map[string]any{
			"hits":       []map[string]any{},
			"confidence": 0.15,
			"reason":     "候选仓库为空，请先完成仓库匹配",
		}), nil
	}

	hits := []localizeHit{}
	for idx, frame := range stack.Frames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		best, ok := matchFrameToCandidates(frame, candidates)
		if !ok {
			continue
		}
		best.FrameIndex = idx
		if best.Line == 0 {
			best.Line = frame.Line
		}
		hits = append(hits, best)
	}

	hits = dedupHits(hits)
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].RepoKey != hits[j].RepoKey {
			return hits[i].RepoKey < hits[j].RepoKey
		}
		if hits[i].Path != hits[j].Path {
			return hits[i].Path < hits[j].Path
		}
		return hits[i].Line < hits[j].Line
	})
	if len(hits) > 30 {
		hits = hits[:30]
	}

	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"repositoryId": h.RepositoryID,
			"repoKey":      h.RepoKey,
			"path":         h.Path,
			"line":         h.Line,
			"reason":       h.Reason,
			"score":        h.Score,
			"frameIndex":   h.FrameIndex,
		})
	}

	confidence := 0.25
	if len(hits) > 0 {
		sum, n := 0.0, 0
		for _, h := range hits {
			if n >= 3 {
				break
			}
			sum += h.Score
			n++
		}
		confidence = round2(clamp01(0.4 + 0.5*(sum/float64(n))))
		if confidence >= 0.95 {
			confidence = 0.95
		}
	}
	return normalizeOutput(map[string]any{
		"hits":       out,
		"confidence": confidence,
	}), nil
}

// matchFrameToCandidates 把单个堆栈帧匹配到最合适的候选仓库。
func matchFrameToCandidates(frame domain.StackFrame, candidates []domain.RepoCandidate) (localizeHit, bool) {
	classNorm := normalizeToken(frame.Class)
	fileBase := normalizeToken(strings.TrimSuffix(baseName(frame.File), "."+extName(frame.File)))
	pkgNorm := normalizeToken(frame.Package)
	methodNorm := normalizeToken(frame.Method)

	best := localizeHit{}
	bestWeight := 0.0
	for _, cand := range candidates {
		weight, reason, path := matchWeight(frame, cand, classNorm, fileBase, pkgNorm, methodNorm)
		if weight <= bestWeight || weight <= 0 {
			continue
		}
		bestWeight = weight
		best = localizeHit{
			RepositoryID: cand.RepositoryID,
			RepoKey:      cand.RepoKey,
			Path:         path,
			Reason:       reason,
			Score:        round2(clamp01(0.7*weight + 0.3*normCandidateScore(cand.Score))),
		}
	}
	if bestWeight <= 0 {
		return localizeHit{}, false
	}
	if best.Path == "" {
		best.Path = frame.File
	}
	return best, true
}

// matchWeight 计算单个候选仓库与堆栈帧的匹配权重与可解释原因。
func matchWeight(frame domain.StackFrame, cand domain.RepoCandidate,
	classNorm, fileBase, pkgNorm, methodNorm string) (weight float64, reason, path string) {

	repoNorm := normalizeToken(cand.RepoKey)
	if repoNorm == "" {
		repoNorm = normalizeToken(cand.Name)
	}

	// ① 类名 ↔ 仓库键
	if classNorm != "" && repoNorm != "" {
		switch {
		case classNorm == repoNorm:
			weight, reason = 0.65, "类 "+frame.Class+" 与仓库键 "+cand.RepoKey+" 归一化后完全一致"
		case len(classNorm) >= 4 && (strings.Contains(classNorm, repoNorm) || strings.Contains(repoNorm, classNorm)):
			weight, reason = 0.5, "类 "+frame.Class+" 与仓库键 "+cand.RepoKey+" 归一化后互相包含"
		}
	}
	// ② 线索文件命中（类名/文件名出现在 hints 路径中）
	for _, hint := range cand.Hints {
		hintNorm := normalizeToken(hint.Path)
		hit := (fileBase != "" && len(fileBase) >= 4 && strings.Contains(hintNorm, fileBase)) ||
			(classNorm != "" && len(classNorm) >= 4 && strings.Contains(hintNorm, classNorm)) ||
			(pkgNorm != "" && len(pkgNorm) >= 6 && strings.Contains(hintNorm, pkgNorm))
		if !hit {
			continue
		}
		w := 0.6
		r := "命中线索文件 " + hint.Path
		if hint.Reason != "" {
			r += "（线索来源：" + hint.Reason + "）"
		}
		if w > weight {
			weight, reason, path = w, r, hint.Path
		}
	}
	// ③ 仓库匹配依据（MatchedBy）
	for _, by := range cand.MatchedBy {
		byNorm := normalizeToken(by)
		if byNorm == "" {
			continue
		}
		hit := (classNorm != "" && len(byNorm) >= 4 && (strings.Contains(classNorm, byNorm) || strings.Contains(byNorm, classNorm))) ||
			(pkgNorm != "" && len(byNorm) >= 6 && (strings.Contains(pkgNorm, byNorm) || strings.Contains(byNorm, pkgNorm))) ||
			(fileBase != "" && len(byNorm) >= 4 && strings.Contains(fileBase, byNorm))
		if !hit {
			continue
		}
		if weight < 0.5 {
			weight = 0.5
			reason = "仓库匹配依据「" + by + "」命中堆栈帧 " + frameDisplay(frame)
		}
	}
	// ④ 方法名兜底（弱证据）
	if weight == 0 && methodNorm != "" && repoNorm != "" && len(methodNorm) >= 4 && strings.Contains(repoNorm, methodNorm) {
		weight = 0.3
		reason = "方法名 " + frame.Method + " 与仓库键 " + cand.RepoKey + " 存在命名关联"
	}
	return weight, reason, path
}

// frameDisplay 生成帧的可读展示文本。
func frameDisplay(frame domain.StackFrame) string {
	switch {
	case frame.Class != "" && frame.Method != "":
		s := frame.Class + "." + frame.Method
		if frame.File != "" {
			s += "(" + baseName(frame.File) + ":" + itoa(frame.Line) + ")"
		}
		return s
	case frame.File != "":
		return baseName(frame.File) + ":" + itoa(frame.Line)
	default:
		return strings.TrimSpace(frame.Raw)
	}
}

// dedupHits 按（仓库 + 文件 + 行）去重，保留最高分。
func dedupHits(hits []localizeHit) []localizeHit {
	best := map[string]localizeHit{}
	order := []string{}
	for _, h := range hits {
		key := h.RepositoryID + "|" + h.RepoKey + "|" + h.Path + "|" + itoa(h.Line)
		if old, ok := best[key]; ok {
			if h.Score > old.Score {
				best[key] = h
			}
			continue
		}
		best[key] = h
		order = append(order, key)
	}
	out := make([]localizeHit, 0, len(order))
	for _, key := range order {
		out = append(out, best[key])
	}
	return out
}

// normalizeToken 归一化标识符：小写、驼峰拆词、去分隔符，用于跨语言模糊匹配。
func normalizeToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = reCamelHead.ReplaceAllString(s, "${1} ${2}")
	s = strings.ToLower(s)
	s = reNonWord.ReplaceAllString(s, "")
	return s
}

// normCandidateScore 归一化候选评分（兼容 0-1 与 0-100 两种量纲）。
func normCandidateScore(score float64) float64 {
	if score > 1 {
		score = score / 100
	}
	return clamp01(score)
}

// clamp01 把数值限制到 [0,1]。
func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// baseName 返回路径最后一段。
func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// extName 返回扩展名（不含点）。
func extName(p string) string {
	b := baseName(p)
	if idx := strings.LastIndex(b, "."); idx >= 0 {
		return b[idx+1:]
	}
	return ""
}

// itoa 无依赖整数转字符串。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
