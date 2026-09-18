// calllog.go 调用流水：记录每次 /v1/chat/completions（及同族端点）实际使用的账号与结果，
// 形成"谁在什么时候被调用、走的哪个域、什么模型、成功还是失败"的记录，供 /calls 查询。
//
// 设计要点：
//   - 请求出口统一记一次（含"全部账号不可用"等失败路径，此时 uid 为空）——只记录事实，
//     不推断原因：失败原因由 status/realm 体现，需要细节时看 stdout 表格日志。
//   - 带 realm 维度：双域部署时能直接分辨一次调用落在 cn 还是 global。
//   - 内存只保留最近 callLogRetention 条，后台每 callLogInterval 原子落盘（与用量统计同节奏）。
package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// CallRecord 一次调用的流水记录。
type CallRecord struct {
	TS       int64  `json:"ts"`                 // Unix 秒
	UID      string `json:"uid,omitempty"`      // 实际使用的账号；全部不可用时为空
	Nickname string `json:"nickname,omitempty"` // 账号昵称
	Realm    string `json:"realm,omitempty"`    // "cn" | "global"
	Model    string `json:"model"`              // 请求模型（已剥 realm 前缀的裸名）
	Mode     string `json:"mode"`               // "stream" | "sync"
	Status   int    `json:"status"`             // HTTP 状态码
}

const (
	// callLogRetention 内存保留的最近条数。
	callLogRetention = 5000
	// callLogInterval 后台落盘周期。
	callLogInterval = 15 * time.Second
	// callLogDefaultLimit /calls 未指定 limit 时的返回条数。
	callLogDefaultLimit = 200
)

// CallTrack 负责调用流水的记录与持久化。
type CallTrack struct {
	mu     sync.Mutex
	path   string // 落盘路径；空串 = 只留内存
	dirty  bool
	Calls  []CallRecord `json:"calls"`
	stopCh chan struct{}
	once   sync.Once
}

// NewCallTrack 构造并加载既有流水（无文件则从零开始），后台周期落盘。
func NewCallTrack(path string) *CallTrack {
	c := &CallTrack{
		path:   path,
		Calls:  []CallRecord{},
		stopCh: make(chan struct{}),
	}
	c.load()
	go c.flusher()
	return c
}

// Record 记录一次调用（uid 为空表示本次请求没有可用账号）。
func (c *CallTrack) Record(uid, nickname, realm, model, mode string, status int) {
	if model == "" {
		model = "-"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls = append(c.Calls, CallRecord{
		TS:       time.Now().Unix(),
		UID:      uid,
		Nickname: nickname,
		Realm:    realm,
		Model:    model,
		Mode:     mode,
		Status:   status,
	})
	if len(c.Calls) > callLogRetention {
		c.Calls = c.Calls[len(c.Calls)-callLogRetention:]
	}
	c.dirty = true
}

// Recent 返回最近 limit 条记录（时间升序）；limit<=0 时返回全部。
func (c *CallTrack) Recent(limit int) []CallRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.Calls)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]CallRecord, limit)
	copy(out, c.Calls[n-limit:])
	return out
}

// Count 返回已保留的记录总数。
func (c *CallTrack) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Calls)
}

func (c *CallTrack) flusher() {
	t := time.NewTicker(callLogInterval)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			c.Flush()
		}
	}
}

// load 从落盘文件恢复历史。
func (c *CallTrack) load() {
	if c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var disk CallTrack
	if err := json.Unmarshal(raw, &disk); err != nil {
		log.Printf("calllog: load parse failed: %v", err)
		return
	}
	if disk.Calls != nil {
		c.Calls = disk.Calls
	}
}

// Flush 脏时原子落盘（tmp + rename）。
func (c *CallTrack) Flush() {
	if c.path == "" {
		return
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	raw, err := json.Marshal(c)
	c.dirty = false
	c.mu.Unlock()
	if err != nil || len(raw) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		log.Printf("calllog: mkdir failed: %v", err)
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("calllog: write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("calllog: rename failed: %v", err)
	}
}

// Close 停止后台落盘并做最后一次 Flush（幂等）。
func (c *CallTrack) Close() {
	c.once.Do(func() { close(c.stopCh) })
	c.Flush()
}

// parseCallLimit 解析 /calls?limit= 查询参数；非法/缺省回落 defaultLimit，上限为内存保留量。
func parseCallLimit(raw string, defaultLimit int) int {
	if raw == "" {
		return defaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultLimit
	}
	if n > callLogRetention {
		return callLogRetention
	}
	return n
}
