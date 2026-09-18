// creditsnap.go 积分快照：周期性采样所有账号的剩余积分（合计 + 每账号明细），
// 形成时间序列，供 /credits/history 展示积分消耗趋势（总余额曲线、消耗速率、各号余额）。
//
// 设计要点：
//   - 采样口径与 /credits 完全一致（upstream.UserResource 逐号聚合）——记录的是
//     真实"可花费余额"，不是缓存或估算值。
//   - 每条快照同时含合计与每账号明细，便于定位是哪个号在消耗；明细同时带 realm，
//     双域部署时可分域看余额。
//   - 内存只保留最近 creditSnapRetention 条，落盘由调用方指定的路径（data 目录）。
//   - 采样间隔默认 creditSnapInterval；总量下降即消耗，上升为补充/签到。
//   - 采样函数返回空切片视为"本次无效"（不写快照）：账号池瞬时为空/上游不可达时
//     不能把 0 余额写进序列，否则趋势图会出现假的断崖。
package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CreditAccountSnapshot 单个账号在同一时刻的剩余积分。
type CreditAccountSnapshot struct {
	UID    string `json:"uid"`
	Name   string `json:"name,omitempty"`
	Realm  string `json:"realm,omitempty"`
	Remain int64  `json:"remain"`
}

// CreditSnapshot 某一时刻的积分快照（合计 + 每账号明细）。
type CreditSnapshot struct {
	TS       int64                   `json:"ts"`       // Unix 秒
	Remain   int64                   `json:"remain"`   // 所有账号剩余积分合计
	Accounts int                     `json:"accounts"` // 成功采样的账号数
	OK       bool                    `json:"ok"`       // 本采样是否有效
	Detail   []CreditAccountSnapshot `json:"detail,omitempty"`
}

const (
	// creditSnapRetention 内存保留的快照条数（5 分钟一条 ≈ 7 天）。
	creditSnapRetention = 2016
	// creditSnapInterval 采样间隔。
	creditSnapInterval = 5 * time.Minute
	// creditSnapFirstDelay 启动后首次采样延迟：避免与网关照启动时的模型拉取/选号争抢，
	// 同时保证重启后不久就有数据点（不必等满一个采样周期）。
	creditSnapFirstDelay = 20 * time.Second
)

// CreditTrack 负责积分快照的采样、留存与落盘。
type CreditTrack struct {
	mu       sync.Mutex
	path     string // 落盘路径；空串 = 只留内存
	dirty    bool
	Snaps    []CreditSnapshot `json:"snapshots"`
	stopCh   chan struct{}
	once     sync.Once
	sampleFn func() []CreditAccountSnapshot
}

// NewCreditTrack 构造并加载既有快照（无文件则从零开始）；sampleFn 可为 nil，
// 之后用 SetSampler 注入（网关启动顺序：先建 track 注入 handler，再回填采样函数）。
func NewCreditTrack(path string, sampleFn func() []CreditAccountSnapshot) *CreditTrack {
	c := &CreditTrack{
		path:     path,
		stopCh:   make(chan struct{}),
		sampleFn: sampleFn,
	}
	c.load()
	go c.loop()
	return c
}

// SetSampler 注入采样函数（构造后设置；传 nil 等价于暂停采样）。
func (c *CreditTrack) SetSampler(fn func() []CreditAccountSnapshot) {
	c.mu.Lock()
	c.sampleFn = fn
	c.mu.Unlock()
}

// loop 后台按 creditSnapInterval 采样（首次延迟 creditSnapFirstDelay）。
func (c *CreditTrack) loop() {
	timer := time.NewTimer(creditSnapFirstDelay)
	defer timer.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-timer.C:
			c.Sample()
			timer.Reset(creditSnapInterval)
		}
	}
}

// Sample 立即采样一次并写入序列；采样函数未注入或返回空明细时不写（见文件头说明）。
func (c *CreditTrack) Sample() {
	c.mu.Lock()
	fn := c.sampleFn
	c.mu.Unlock()
	if fn == nil {
		return
	}
	detail := fn()
	if len(detail) == 0 {
		return
	}
	var remain int64
	for _, d := range detail {
		remain += d.Remain
	}
	c.Add(CreditSnapshot{
		TS:       time.Now().Unix(),
		Remain:   remain,
		Accounts: len(detail),
		OK:       true,
		Detail:   detail,
	})
}

// Add 追加一条快照（供采样与测试使用）；OK=false 的条目直接丢弃。
func (c *CreditTrack) Add(s CreditSnapshot) {
	if !s.OK {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 同一秒去重（避免手动触发与定时采样叠加出重复点）。
	if n := len(c.Snaps); n > 0 && c.Snaps[n-1].TS == s.TS {
		c.Snaps[n-1] = s
	} else {
		c.Snaps = append(c.Snaps, s)
	}
	if len(c.Snaps) > creditSnapRetention {
		c.Snaps = c.Snaps[len(c.Snaps)-creditSnapRetention:]
	}
	c.dirty = true
}

// History 返回快照序列拷贝（时间升序）。
func (c *CreditTrack) History() []CreditSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CreditSnapshot, len(c.Snaps))
	copy(out, c.Snaps)
	return out
}

// load 从落盘文件恢复历史。
func (c *CreditTrack) load() {
	if c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var disk struct {
		Snapshots []CreditSnapshot `json:"snapshots"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		log.Printf("creditsnap: load parse failed: %v", err)
		return
	}
	if disk.Snapshots != nil {
		c.Snaps = disk.Snapshots
	}
}

// Flush 脏时原子落盘（tmp + rename）。
func (c *CreditTrack) Flush() {
	if c.path == "" {
		return
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	raw, err := json.Marshal(map[string]any{"snapshots": c.Snaps})
	c.dirty = false
	c.mu.Unlock()
	if err != nil || len(raw) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		log.Printf("creditsnap: mkdir failed: %v", err)
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("creditsnap: write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("creditsnap: rename failed: %v", err)
	}
}

// Close 停止后台采样并落盘（幂等）。
func (c *CreditTrack) Close() {
	c.once.Do(func() { close(c.stopCh) })
	c.Flush()
}
