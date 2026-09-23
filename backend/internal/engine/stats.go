package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/store"
)

// DayBucket 单日任务量趋势桶（供前端 ECharts 趋势图直接消费）。
type DayBucket struct {
	// Date 日期，格式 2006-01-02（本地时区）。
	Date string `json:"date"`
	// Total 当日运行总数。
	Total int `json:"total"`
	// Succeeded 当日成功数（succeeded）。
	Succeeded int `json:"succeeded"`
	// Failed 当日失败数（failed）。
	Failed int `json:"failed"`
	// Degraded 当日降级完成数（degraded）。
	Degraded int `json:"degraded"`
}

// BuildEngineStats 聚合租户维度的调度层统计。
//
// 统计口径：
//   - Running  = analyzing + repairing + verifying（正在执行）
//   - Queued   = queued（已受理未开始）
//   - Total    = 该租户全部运行记录数
//   - SuccessRate = succeeded / Total（0-1 的比例，Total 为 0 时取 0）
//   - AvgElapsedMS = 仅统计已结束（EndedAt 非零）的运行，避免运行中任务的耗时污染均值
//   - RepoSwitches = 各运行 Resolution 长度累加（每次仓库版本锁定计一次）
//   - DegradedRuns = Degraded 为真的运行数
func BuildEngineStats(st *store.Store, tenantID string) (*domain.EngineStats, error) {
	if st == nil {
		return nil, fmt.Errorf("数据访问层不能为空，无法聚合运行统计")
	}
	runs := st.AllRuns(tenantID)
	byState := st.CountRunsByState(tenantID)

	stats := &domain.EngineStats{
		Total:      len(runs),
		ByState:    map[domain.TaskState]int{},
		BySeverity: map[domain.Severity]int{},
	}
	for state, n := range byState {
		stats.ByState[state] = n
	}
	stats.Running = byState[domain.StateAnalyzing] + byState[domain.StateRepairing] + byState[domain.StateVerifying]
	stats.Queued = byState[domain.StateQueued]

	var (
		elapsedSum   int64
		elapsedCount int64
		succeeded    int
		repoSwitch   int64
		degraded     int64
	)
	for _, r := range runs {
		if r.Severity != "" {
			stats.BySeverity[r.Severity]++
		}
		if r.State == domain.StateSucceeded {
			succeeded++
		}
		if !r.EndedAt.IsZero() {
			elapsedSum += r.ElapsedMS()
			elapsedCount++
		}
		repoSwitch += int64(len(r.Resolution))
		if r.Degraded {
			degraded++
		}
	}
	if stats.Total > 0 {
		stats.SuccessRate = round4(float64(succeeded) / float64(stats.Total))
	}
	if elapsedCount > 0 {
		stats.AvgElapsedMS = elapsedSum / elapsedCount
	}
	stats.RepoSwitches = repoSwitch
	stats.DegradedRuns = degraded
	return stats, nil
}

// OverviewByDay 按天聚合近 N 天的任务量趋势（含无数据的空桶，保证趋势图连续）。
//
// 统计口径：按运行创建时间的本地日期归桶；Succeeded/Failed/Degraded 分别对应
// succeeded / failed / degraded 终态；queued 与 needs_review、cancelled 只计入 Total。
// days <= 0 时默认取 7 天。
func OverviewByDay(runs []domain.TaskRun, days int) []DayBucket {
	if days <= 0 {
		days = 7
	}
	if days > 365 {
		days = 365
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	index := make(map[string]*DayBucket, days)
	out := make([]DayBucket, 0, days)
	for i := days - 1; i >= 0; i-- {
		date := today.AddDate(0, 0, -i).Format("2006-01-02")
		out = append(out, DayBucket{Date: date})
	}
	for i := range out {
		index[out[i].Date] = &out[i]
	}

	for _, r := range runs {
		at := r.CreatedAt
		if at.IsZero() {
			at = r.UpdatedAt
		}
		if at.IsZero() {
			continue
		}
		key := at.In(now.Location()).Format("2006-01-02")
		bucket, ok := index[key]
		if !ok {
			continue
		}
		bucket.Total++
		switch r.State {
		case domain.StateSucceeded:
			bucket.Succeeded++
		case domain.StateFailed:
			bucket.Failed++
		case domain.StateDegraded:
			bucket.Degraded++
		}
	}
	return out
}

// TopDegradedRuns 返回最近的降级运行（供可观测页快速定位证据缺口）。
func TopDegradedRuns(runs []domain.TaskRun, limit int) []domain.RunBrief {
	if limit <= 0 {
		limit = 10
	}
	out := []domain.RunBrief{}
	sorted := append([]domain.TaskRun{}, runs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.After(sorted[j].CreatedAt) })
	for _, r := range sorted {
		if !r.Degraded {
			continue
		}
		summary := ""
		if r.RootCause != nil {
			summary = r.RootCause.Summary
		}
		out = append(out, domain.RunBrief{
			RunID: r.ID, Title: r.Title, State: r.State, Severity: r.Severity,
			Summary: summary, At: r.CreatedAt,
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}
