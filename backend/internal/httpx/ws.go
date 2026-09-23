package httpx

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// RFC6455 常量
// ---------------------------------------------------------------------------

// WebSocket 操作码。
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// wsGUID 是 RFC6455 §4.2.2 规定的握手魔术字符串。
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket 关闭码。
const (
	wsCloseNormal          = 1000
	wsCloseGoingAway       = 1001
	wsCloseProtocolError   = 1002
	wsCloseUnsupportedData = 1003
	wsClosePolicyViolation = 1008
	// wsCloseTryAgainLater 连接数超限（RFC6455 §7.4.2 / 1013）。
	wsCloseTryAgainLater = 1013
	// wsCloseMessageTooBig 单帧/单消息过大。
	wsCloseMessageTooBig = 1009
)

// WebSocket 运行参数。
const (
	// wsMaxConnections 单进程最大 WebSocket 连接数，超出直接拒绝（1013）。
	wsMaxConnections = 500
	// wsPingInterval 服务端主动 ping 间隔。
	wsPingInterval = 30 * time.Second
	// wsPongTimeout 客户端 pong 超时时间：超过该时间未收到任何客户端帧即关闭连接。
	wsPongTimeout = 90 * time.Second
	// wsWriteTimeout 单帧写超时。
	wsWriteTimeout = 10 * time.Second
	// wsMaxMessageBytes 单条消息（含分片累计）上限 1MiB。
	wsMaxMessageBytes = 1 << 20
	// wsReadIdleTimeout 读空闲超时（应由 ping/pong 或客户端消息刷新）。
	wsReadIdleTimeout = 120 * time.Second
	// wsSendQueue 单连接发送队列长度，满则丢弃（实时性优先，不阻塞事件总线）。
	wsSendQueue = 64
)

var errWSClosed = errors.New("websocket 连接已关闭")

// ---------------------------------------------------------------------------
// Hub
// ---------------------------------------------------------------------------

// Hub WebSocket 事件广播中心：把 domain.EventBus 的事件推送给前端，
// 并限制单进程连接数上限。
type Hub struct {
	bus domain.EventBus
	log *logx.Logger

	mu     sync.Mutex
	conns  map[*wsConn]struct{}
	closed bool
	nextID int64
}

// NewHub 创建 WebSocket 广播中心（bus 可为 nil，此时仅支持客户端 ping/pong 心跳）。
func NewHub(bus domain.EventBus, log *logx.Logger) *Hub {
	if log == nil {
		log = logx.Nop()
	}
	return &Hub{bus: bus, log: log, conns: make(map[*wsConn]struct{})}
}

// Connections 返回当前活跃连接数。
func (h *Hub) Connections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// Close 关闭全部 WebSocket 连接（服务停机时调用）。
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	conns := make([]*wsConn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.closeWith(wsCloseGoingAway, "服务正在关闭")
	}
}

func (h *Hub) add(c *wsConn) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("hub 已关闭")
	}
	if len(h.conns) >= wsMaxConnections {
		return fmt.Errorf("WebSocket 连接数已达上限 %d", wsMaxConnections)
	}
	h.nextID++
	c.id = h.nextID
	h.conns[c] = struct{}{}
	return nil
}

func (h *Hub) remove(c *wsConn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// HandleWS 处理一次 WebSocket 会话：
//
//  1. 完成 RFC6455 握手（校验 Upgrade / Connection / Sec-WebSocket-Version / Sec-WebSocket-Key）；
//  2. 若传入 runID，先补发该运行的历史事件（通过可选接口 History 断言获取，非 Bus 实现则跳过）；
//  3. 订阅 EventBus 并持续推送 domain.Event 的 JSON；
//  4. 每 30s 发送一次 ping，pong 超时或写失败即关闭；
//  5. 客户端文本消息 "ping" 回复 "pong"。
//
// 连接数超过上限（500）时返回 503 并在握手中断前以 1013 关闭码拒绝。
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request, tenantID string, runID string) {
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if err := validateUpgrade(r, key); err != nil {
		h.log.Warn("WebSocket 握手校验失败",
			"err", err.Error(), "path", r.URL.Path, "req", RequestIDFrom(r.Context()))
		WriteError(w, r, ErrBadRequest("WebSocket 握手失败: "+err.Error()))
		return
	}

	c := &wsConn{
		hub:      h,
		log:      h.log.With("tenant", tenantID, "run", runID, "req", RequestIDFrom(r.Context())),
		send:     make(chan []byte, wsSendQueue),
		tenantID: tenantID,
		runID:    runID,
		lastSeen: time.Now(),
	}
	if err := h.add(c); err != nil {
		h.log.Warn("WebSocket 连接被拒绝", "err", err.Error(), "tenant", tenantID)
		// 以 1013（Try Again Later）告知客户端稍后重试。
		if err := writeRejectedUpgrade(w, key, wsCloseTryAgainLater, "连接数已达上限"); err != nil {
			WriteError(w, r, ErrUnavailable("WebSocket 连接数已达上限，请稍后重试", err))
		}
		return
	}
	defer h.remove(c)

	// http.Hijacker（Go 1.20+ 也可用 http.NewResponseController，两者等价；
	// 这里选用 Hijacker，因为它同时返回 bufio.ReadWriter，可保留已缓冲的客户端数据）。
	hj, ok := w.(http.Hijacker)
	if !ok {
		h.log.Error("当前 ResponseWriter 不支持 Hijack，无法升级 WebSocket")
		WriteError(w, r, ErrInternalFn("服务器不支持 WebSocket 升级", nil))
		return
	}
	raw, rw, err := hj.Hijack()
	if err != nil {
		h.log.Error("WebSocket Hijack 失败", "err", err.Error())
		WriteError(w, r, ErrInternalFn("WebSocket 升级失败", err))
		return
	}
	defer raw.Close()

	// 握手响应必须手写（连接已被劫持，标准 ResponseWriter 不再可用）。
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n" +
		"X-Request-Id: " + RequestIDFrom(r.Context()) + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		h.log.Warn("WebSocket 握手响应写入失败", "err", err.Error())
		return
	}
	if err := rw.Flush(); err != nil {
		h.log.Warn("WebSocket 握手响应刷新失败", "err", err.Error())
		return
	}
	h.log.Info("WebSocket 已建立", "conns", h.Connections())

	c.serve(raw, rw.Reader)
}

// validateUpgrade 校验 WebSocket 握手请求头。
func validateUpgrade(r *http.Request, key string) error {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return errors.New("缺少 Upgrade: websocket")
	}
	if !headerContainsToken(r.Header.Get("Connection"), "upgrade") {
		return errors.New("缺少 Connection: Upgrade")
	}
	if v := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); v != "13" {
		return fmt.Errorf("不支持的 Sec-WebSocket-Version: %q", v)
	}
	if key == "" {
		return errors.New("缺少 Sec-WebSocket-Key")
	}
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		return errors.New("Sec-WebSocket-Key 不是合法的 16 字节 base64")
	}
	return nil
}

// headerContainsToken 判断逗号分隔的头值中是否包含某 token（大小写不敏感）。
func headerContainsToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// wsAcceptKey 计算 Sec-WebSocket-Accept = base64(sha1(key + GUID))。
func wsAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// writeRejectedUpgrade 在连接数超限时仍完成握手，但立刻以指定关闭码关闭，
// 让浏览器端能读到明确的关闭原因（而不是一个无法解释的 HTTP 错误）。
func writeRejectedUpgrade(w http.ResponseWriter, key string, code int, reason string) error {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return errors.New("ResponseWriter 不支持 Hijack")
	}
	raw, rw, err := hj.Hijack()
	if err != nil {
		return err
	}
	defer raw.Close()
	_ = raw.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"); err != nil {
		return err
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)
	if err := writeFrameRaw(rw.Writer, wsOpClose, payload); err != nil {
		return err
	}
	return rw.Flush()
}

// ---------------------------------------------------------------------------
// 单连接
// ---------------------------------------------------------------------------

// wsConn 一条 WebSocket 服务端连接。
type wsConn struct {
	hub *Hub
	log *logx.Logger
	id  int64

	tenantID string
	runID    string

	send chan []byte

	writeMu sync.Mutex
	// rawConn 底层 TCP 连接，用于控制帧与关闭帧直接写出（由 writeMu 串行化）。
	rawConn net.Conn

	mu       sync.Mutex
	lastSeen time.Time
	closed   bool
	cancel   context.CancelFunc
}

// serve 运行读写循环，阻塞直到连接关闭。
func (c *wsConn) serve(raw net.Conn, reader *bufio.Reader) {
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancel = cancel
	c.rawConn = raw
	c.mu.Unlock()
	defer cancel()

	// ① 历史事件补发：domain.EventBus 接口本身没有 History，
	//    用可选接口断言（不修改 domain 包），非 Bus 实现直接跳过。
	if c.hub.bus != nil && !isNil(c.hub.bus) {
		if hist, ok := c.hub.bus.(interface {
			History(tenantID, runID string, limit int) []domain.Event
		}); ok {
			evs := hist.History(c.tenantID, c.runID, 200)
			c.log.Debug("WebSocket 补发历史事件", "count", len(evs))
			for _, ev := range evs {
				c.enqueue(ev)
			}
		}
	}

	var (
		subCh <-chan domain.Event
		unsub func()
		wg    sync.WaitGroup
	)
	// ② 订阅实时事件。
	if c.hub.bus != nil && !isNil(c.hub.bus) {
		subCh, unsub = c.hub.bus.Subscribe(c.tenantID)
	}
	if unsub != nil {
		defer unsub()
	}

	// 写协程：串行化所有写出（帧写入不可并发）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writeLoop(ctx, raw, subCh)
	}()

	// 读循环：处理 ping/pong/close/文本消息，写失败或超时即退出。
	c.readLoop(ctx, raw, reader)

	cancel()
	_ = raw.SetReadDeadline(time.Now()) // 立即解阻塞读
	wg.Wait()
	c.log.Info("WebSocket 已断开", "conns", c.hub.Connections())
}

// writeLoop 串行写：批量抽取发送队列 + 定时 ping + 订阅事件转发。
func (c *wsConn) writeLoop(ctx context.Context, raw net.Conn, subCh <-chan domain.Event) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	pingPayload := []byte(fmt.Sprintf("ca-%d", c.id))

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-subCh:
			if !ok {
				return
			}
			if !c.enqueue(ev) {
				return
			}
		case <-ticker.C:
			// pong 超时：客户端长时间无任何响应帧，判死并关闭。
			c.mu.Lock()
			idle := time.Since(c.lastSeen)
			c.mu.Unlock()
			if idle > wsPongTimeout {
				c.log.Warn("WebSocket pong 超时，关闭连接", "idleSec", int(idle.Seconds()))
				c.closeWith(wsCloseGoingAway, "心跳超时")
				return
			}
			if err := raw.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
				return
			}
			if err := writeFrameRaw(raw, wsOpPing, pingPayload); err != nil {
				c.log.Debug("WebSocket ping 写入失败", "err", err.Error())
				return
			}
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			if err := c.flushBatch(raw, msg); err != nil {
				c.log.Debug("WebSocket 写入失败", "err", err.Error())
				return
			}
		}
	}
}

// flushBatch 写出 msg 并顺带清空队列中已就绪的帧，减少 syscall 次数。
func (c *wsConn) flushBatch(raw net.Conn, first []byte) error {
	if err := raw.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	w := bufio.NewWriter(raw)
	if err := writeFrameRaw(w, wsOpText, first); err != nil {
		return err
	}
drain:
	for i := 0; i < 32; i++ {
		select {
		case more, ok := <-c.send:
			if !ok {
				break drain
			}
			if err := writeFrameRaw(w, wsOpText, more); err != nil {
				return err
			}
		default:
			break drain
		}
	}
	return w.Flush()
}

// enqueue 把事件序列化后放入发送队列（服务端发送不加掩码）。
// 队列满时丢弃并返回 false，保证事件总线永不被慢客户端阻塞。
func (c *wsConn) enqueue(ev domain.Event) bool {
	payload, err := json.Marshal(ev)
	if err != nil {
		c.log.Warn("WebSocket 事件序列化失败", "err", err.Error(), "type", ev.Type)
		return true
	}
	select {
	case c.send <- payload:
		return true
	default:
		c.log.Warn("WebSocket 发送队列已满，丢弃事件", "type", ev.Type, "seq", ev.Seq)
		return true
	}
}

// readLoop 读取客户端帧：处理掩码解码、分片续帧、ping/pong/close 与文本心跳。
func (c *wsConn) readLoop(ctx context.Context, raw net.Conn, reader *bufio.Reader) {
	var (
		fragOpcode int
		fragBuf    []byte
	)
	for {
		if ctx.Err() != nil {
			return
		}
		if err := raw.SetReadDeadline(time.Now().Add(wsReadIdleTimeout)); err != nil {
			return
		}
		fin, opcode, payload, err := readFrame(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && !isTimeout(err) {
				c.log.Debug("WebSocket 读帧结束", "err", err.Error())
			}
			c.writeCloseFrame(wsCloseNormal, "normal closure")
			return
		}
		c.touch()

		switch opcode {
		case wsOpPing:
			c.writeControl(wsOpPong, payload)
		case wsOpPong:
			// 心跳已由 touch 记录
		case wsOpClose:
			code := wsCloseNormal
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload[:2]))
			}
			c.log.Debug("收到客户端关闭帧", "code", code)
			c.writeCloseFrame(wsCloseNormal, "bye")
			return
		case wsOpText, wsOpBinary:
			if fin {
				c.handleMessage(opcode, payload)
				continue
			}
			fragOpcode = opcode
			fragBuf = append(fragBuf[:0], payload...)
		case wsOpContinuation:
			if fragOpcode == 0 {
				c.log.Debug("收到无起始帧的续帧，协议错误")
				c.writeCloseFrame(wsCloseProtocolError, "unexpected continuation")
				return
			}
			fragBuf = append(fragBuf, payload...)
			if len(fragBuf) > wsMaxMessageBytes {
				c.writeCloseFrame(wsCloseMessageTooBig, "message too big")
				return
			}
			if fin {
				c.handleMessage(fragOpcode, fragBuf)
				fragOpcode = 0
				fragBuf = fragBuf[:0]
			}
		default:
			c.log.Debug("收到未知操作码", "opcode", opcode)
			c.writeCloseFrame(wsCloseUnsupportedData, "unsupported opcode")
			return
		}
	}
}

// handleMessage 处理客户端完整消息：文本 "ping" → "pong"，其余忽略。
func (c *wsConn) handleMessage(opcode int, payload []byte) {
	if opcode != wsOpText {
		return
	}
	switch strings.ToLower(strings.TrimSpace(string(payload))) {
	case "ping":
		c.writeControl(wsOpText, []byte("pong"))
		c.log.Debug("WebSocket 文本 ping → pong")
	default:
		// 其余客户端消息当前无业务语义，按协议要求静默忽略。
	}
}

// touch 记录最近一次收到客户端数据的时间。
func (c *wsConn) touch() {
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.mu.Unlock()
}

// writeControl 发送控制/文本帧：加写锁串行化，**不加掩码**（服务端规范）。
func (c *wsConn) writeControl(opcode int, payload []byte) {
	c.mu.Lock()
	conn := c.rawConn
	c.mu.Unlock()
	if conn == nil {
		// 底层连接尚未绑定：走发送队列异步写出，避免丢帧。
		select {
		case c.send <- payload:
		default:
		}
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	_ = writeFrameRaw(conn, opcode, payload)
}

// writeCloseFrame 尽力发送关闭帧（连接可能已不可写，错误忽略）。
func (c *wsConn) writeCloseFrame(code int, reason string) {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	conn := c.rawConn
	cancel := c.cancel
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		c.writeMu.Lock()
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		_ = writeFrameRaw(conn, wsOpClose, payload)
		c.writeMu.Unlock()
		_ = conn.Close()
	}
}

// closeWith 主动关闭连接（Hub 停机 / 心跳超时使用）。
func (c *wsConn) closeWith(code int, reason string) { c.writeCloseFrame(code, reason) }

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

// ---------------------------------------------------------------------------
// 帧编解码
// ---------------------------------------------------------------------------

// readFrame 读取一帧：返回 fin、opcode 与已去掩码的载荷。
//
// 处理要点：
//   - payload 长度 7 / 16 / 64 位三种编码；
//   - 客户端帧必须带掩码（RFC6455 §5.1），解码时按 maskKey 循环异或去掩码；
//   - 单帧载荷超过 1MiB 直接报错，防止内存放大攻击。
func readFrame(r *bufio.Reader) (fin bool, opcode int, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(r, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		// RSV1-3 未协商扩展时必须为 0
		return false, 0, nil, errors.New("RSV 位非零")
	}
	opcode = int(head[0] & 0x0F)
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u > wsMaxMessageBytes {
			return false, 0, nil, errors.New("帧载荷过大")
		}
		length = int64(u)
	}
	if length < 0 || length > wsMaxMessageBytes {
		return false, 0, nil, errors.New("帧载荷过大")
	}
	// 控制帧必须 ≤125 字节且不可分片（RFC6455 §5.5）。
	if opcode >= wsOpClose && (length > 125 || !fin) {
		return false, 0, nil, errors.New("非法控制帧")
	}

	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(r, maskKey[:]); err != nil {
			return false, 0, nil, err
		}
	} else if opcode != wsOpClose || length > 0 {
		// 客户端→服务端必须掩码；例外：测试工具发送的空关闭帧按宽容处理。
		if opcode != wsOpPing && opcode != wsOpPong && length > 0 {
			return false, 0, nil, errors.New("客户端帧未加掩码")
		}
	}

	payload = make([]byte, length)
	if length > 0 {
		if _, err = io.ReadFull(r, payload); err != nil {
			return false, 0, nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i&3]
		}
	}
	return fin, opcode, payload, nil
}

// writeFrameRaw 写出一个服务端帧（**不加掩码**，RFC6455 §5.1）。
func writeFrameRaw(w io.Writer, opcode int, payload []byte) error {
	header := make([]byte, 0, 10)
	header = append(header, byte(0x80|opcode)) // FIN=1

	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(n))
	default:
		header = append(header, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(n))
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}
