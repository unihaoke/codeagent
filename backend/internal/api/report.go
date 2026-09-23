package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
	"github.com/codeagent/backend/internal/store"
)

// sseKeepaliveInterval SSE 注释心跳间隔（15s，防止中间代理断流）。
const sseKeepaliveInterval = 15 * time.Second

// runHistoryProvider 可选历史事件能力（domain.EventBus 接口本身没有 History）。
//
// 通过类型断言获取，非该形态的实现（例如纯通道型 fake bus）自动跳过历史回放，
// 从而在不修改 domain 包的前提下兼容 internal/platform/eventbus.Bus。
type runHistoryProvider interface {
	History(tenantID, runID string, limit int) []domain.Event
}

// handleRunEvents GET /api/v1/runs/{runId}/events —— 运行时事件（历史回放 + SSE 实时流）。
//
// 行为：
//  1. 设置 SSE 必需响应头（Cache-Control: no-cache / Connection: keep-alive / X-Accel-Buffering: no）；
//  2. 先补发 Bus.History 的历史事件（可选接口断言）；
//  3. 订阅 Bus 实时事件，按 `data: <json>\n\n` 推送；
//  4. 每 15s 发送 `: keepalive\n\n` 注释心跳；
//  5. 客户端断开（r.Context().Done()）时取消订阅并退出；
//  6. 收到 task.terminal 事件后主动收流（前端据此关闭连接）。
//
// 降级：d.Bus 为 nil 时退化为一次性 JSON 返回（含 degraded 说明）。
func (d *Deps) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "run.events")
	tenantID := subject(r).TenantID
	runID := pathValue(r, "runId")
	if runID == "" {
		end(d, r, reqID, "run.events", errors.New("runId 为空"))
		writeError(w, r, errBadRequest("runId 为必填"))
		return
	}

	if d.Bus == nil || isNilInterface(d.Bus) {
		end(d, r, reqID, "run.events", errors.New("bus 未装配，SSE 降级"))
		httpx.WriteJSON(w, r, map[string]any{
			"degraded": true,
			"reason":   "事件总线未装配，实时事件流不可用（已降级为一次性返回）",
			"runId":    runID,
			"events":   []domain.Event{},
		})
		return
	}

	// 先校验运行存在且属于当前租户，避免对不存在的 run 长时间挂住连接。
	if _, err := d.provider().GetRun(r.Context(), tenantID, runID); err != nil {
		end(d, r, reqID, "run.events", err)
		writeError(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // 关闭 Nginx 缓冲，避免事件被攒批
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 首个注释帧立即刷出，避免代理等待首字节超时。
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flush()

	// ① 历史事件回放（可选接口断言，不修改 domain 包）。
	if hist, ok := d.Bus.(runHistoryProvider); ok {
		for _, ev := range hist.History(tenantID, runID, 500) {
			if err := writeSSE(w, ev); err != nil {
				end(d, r, reqID, "run.events", err)
				return
			}
		}
		flush()
	}

	// ② 实时订阅；defer cancel 保证断开即取消订阅，避免 goroutine/通道泄漏。
	ch, cancel := d.Bus.Subscribe(tenantID)
	defer cancel()

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	ctx := r.Context()
	d.log().Debug("SSE 已建立", "req", reqID, "run", runID, "tenant", tenantID)

	for {
		select {
		case <-ctx.Done():
			// 客户端断开：defer cancel 已保证清理。
			end(d, r, reqID, "run.events", ctx.Err())
			return
		case ev, ok := <-ch:
			if !ok {
				end(d, r, reqID, "run.events", nil)
				return
			}
			// 单次运行流只推送本 run 的事件（tenantID 订阅已保证租户隔离）。
			if runID != "" && ev.RunID != "" && ev.RunID != runID {
				continue
			}
			if err := writeSSE(w, ev); err != nil {
				end(d, r, reqID, "run.events", err)
				return
			}
			flush()
			if ev.Type == "task.terminal" {
				end(d, r, reqID, "run.events", nil)
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				end(d, r, reqID, "run.events", err)
				return
			}
			flush()
		}
	}
}

// writeSSE 以 `data: <json>\n\n` 写出一条事件，保留 `id:` 供断线续传（Last-Event-ID）。
//
// 注意：不写 `event:` 字段。浏览器 EventSource 只在未指定 event 名时投递到 onmessage，
// 一旦带 event 名就必须用 addEventListener 逐个注册，否则事件被静默丢弃（表现为
// 阶段进度条不推进、状态不更新）。统一走默认 message 通道，客户端无需维护事件名白名单。
func writeSSE(w http.ResponseWriter, ev domain.Event) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, raw)
	return err
}

// ---------------------------------------------------------------------------
// 报告
// ---------------------------------------------------------------------------

// handleListReports GET /api/v1/reports —— 报告列表（分页/关键字）。
func (d *Deps) handleListReports(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "report.list")
	page, err := d.provider().ListReports(r.Context(), subject(r).TenantID, pageQuery(r))
	if err != nil {
		end(d, r, reqID, "report.list", err)
		writeError(w, r, err)
		return
	}
	httpx.WritePage(w, r, page)
	end(d, r, reqID, "report.list", nil)
}

// handleGetReport GET /api/v1/reports/{id} —— 报告详情（含 Markdown 正文）。
func (d *Deps) handleGetReport(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "report.get")
	id := pathValue(r, "id")
	if id == "" {
		end(d, r, reqID, "report.get", errors.New("id 为空"))
		writeError(w, r, errBadRequest("id 为必填"))
		return
	}
	rep, ok := d.Store.GetReport(subject(r).TenantID, id)
	if !ok {
		end(d, r, reqID, "report.get", store.ErrNotFound)
		writeError(w, r, errNotFoundFn("报告不存在"))
		return
	}
	httpx.WriteJSON(w, r, rep)
	end(d, r, reqID, "report.get", nil)
}

// handleReportMarkdown GET /api/v1/reports/{id}/markdown —— 纯文本 Markdown。
//
// Content-Type 固定为 text/markdown; charset=utf-8；`?download=1` 时附带
// Content-Disposition 触发浏览器下载。响应体直接输出 rep.Markdown 原文（不做信封包装）。
func (d *Deps) handleReportMarkdown(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "report.markdown")
	id := pathValue(r, "id")
	if id == "" {
		end(d, r, reqID, "report.markdown", errors.New("id 为空"))
		writeError(w, r, errBadRequest("id 为必填"))
		return
	}
	rep, ok := d.Store.GetReport(subject(r).TenantID, id)
	if !ok {
		end(d, r, reqID, "report.markdown", store.ErrNotFound)
		writeError(w, r, errNotFoundFn("报告不存在"))
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("X-Request-Id", reqID)
	w.Header().Set("Cache-Control", "no-cache")
	if queryBool(r, "download") {
		filename := sanitizeFilename(rep.Title)
		if filename == "" {
			filename = rep.ID
		}
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", filename+".md", urlEscape(filename+".md")))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(rep.Markdown))
	end(d, r, reqID, "report.markdown", nil)
}

// sanitizeFilename 清洗文件名，避免响应头注入与非法字符。
func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' ||
			r == '<' || r == '>' || r == '|' || r == '\r' || r == '\n' || r == '\t':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len([]rune(out)) > 80 {
		out = string([]rune(out)[:80])
	}
	return out
}

// urlEscape 对文件名做 RFC5987 转义（仅保留 ASCII 可见字符，其余 %XX）。
func urlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
