// stats.go 看板 token 用量统计：累计总量 + 分模型 + 分账号 + 分域(realm) + 按小时趋势。
//
// 定位：/stats 接口的数据源，供看板回答"哪个模型/哪个账号/哪个域在消耗多少 token"。
//
// 设计要点：
//   - 仅在请求出口记录一次（含失败路径）；只有 status==200 且拿到 usage 的请求计入，
//     失败请求没有用量语义，不计入也不占 Requests 计数。
//   - 四个维度互相独立（模型 / 账号 / 域 / 小时），一次记录同时累加，便于任意切片。
//   - realm 维度是双域部署的核心观察项：同一个模型名在 cn 与 global 上的消耗分开可看。
//   - 内存累积 + 后台每 statsFlushInterval 原子落盘（tmp + rename）；进程重启延续历史。
//   - 按小时桶只保留最近 statsHourlyRetention，避免文件随运行时间无界膨胀。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// UsageTotals 单一维度上的 token 累计。
type UsageTotals struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Total      int64 `json:"total"`
	Requests   int64 `json:"requests"`
}

// add 累加一次请求的用量。负值按 0 处理：上游未回 usage 时网关以 -1 表示"缺失"，
// 缺失不能被当成负数冲减累计。
func (u *UsageTotals) add(prompt, completion, total int64) {
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	if total < 0 {
		total = 0
	}
	u.Prompt += prompt
	u.Completion += completion
	u.Total += total
	u.Requests++
}

const (
	// statsHourlyRetention 按小时桶保留时长（超出即裁剪，控制文件体积）。
	statsHourlyRetention = 14 * 24 * time.Hour
	// statsFlushInterval 后台落盘周期。
	statsFlushInterval = 15 * time.Second
	// statsEmptyDim 维度值为空时的占位（与日志表格的 "-" 口径一致，不伪造具体值）。
	statsEmptyDim = "-"
)

// Stats 用量统计器：内存累积 + 周期落盘。零值不可用，须经 NewStats 构造。
type Stats struct {
	mu    sync.Mutex
	path  string // 落盘路径；空串 = 只留内存（不落盘，供测试）
	dirty bool

	Total   UsageTotals            `json:"total"`
	ByModel map[string]UsageTotals `json:"by_model"`
	ByUID   map[string]UsageTotals `json:"by_uid"`
	ByRealm map[string]UsageTotals `json:"by_realm"`
	Hourly  map[string]UsageTotals `json:"hourly"` // key: 2006-01-02T15（本地时区）

	stopCh chan struct{}
	once   sync.Once
}

// NewStats 构造并加载既有统计（无文件则从零开始），后台按 statsFlushInterval 落盘。
func NewStats(path string) *Stats {
	s := &Stats{
		path:    path,
		ByModel: map[string]UsageTotals{},
		ByUID:   map[string]UsageTotals{},
		ByRealm: map[string]UsageTotals{},
		Hourly:  map[string]UsageTotals{},
		stopCh:  make(chan struct{}),
	}
	s.load()
	go s.flusher()
	return s
}

// Record 记录一次请求的 token 用量。model/realm 为空时以 "-" 归桶；uid 为空
// （全部账号不可用等失败路径）不产生分账号条目。
func (s *Stats) Record(model, uid, realm string, prompt, completion, total, status int) {
	if status != http.StatusOK || (prompt <= 0 && completion <= 0 && total <= 0) {
		return
	}
	totalUse := int64(total)
	if totalUse <= 0 {
		totalUse = int64(prompt) + int64(completion)
	}
	p, c := int64(prompt), int64(completion)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	s.Total.add(p, c, totalUse)
	accumulate(s.ByModel, dimOr(model, statsEmptyDim), p, c, totalUse)
	accumulate(s.ByRealm, dimOr(realm, statsEmptyDim), p, c, totalUse)
	if uid != "" {
		accumulate(s.ByUID, uid, p, c, totalUse)
	}
	accumulate(s.Hourly, now.Format("2006-01-02T15"), p, c, totalUse)

	s.pruneLocked(now)
	s.dirty = true
}

// accumulate 向 map 的某个维度键累加一次用量。
func accumulate(m map[string]UsageTotals, key string, prompt, completion, total int64) {
	v := m[key]
	v.add(prompt, completion, total)
	m[key] = v
}

// dimOr 维度值为空时回落占位符。
func dimOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// pruneLocked 裁剪超出保留期的按小时桶（key 为可字典序比较的时间串）。
func (s *Stats) pruneLocked(now time.Time) {
	cutoff := now.Add(-statsHourlyRetention).Format("2006-01-02T15")
	for k := range s.Hourly {
		if k < cutoff {
			delete(s.Hourly, k)
		}
	}
}

// flusher 周期落盘（仅在 dirty 时真正写盘）。
func (s *Stats) flusher() {
	t := time.NewTicker(statsFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.Flush()
		}
	}
}

// Flush 脏时原子落盘（tmp + rename，避免半截文件）。
func (s *Stats) Flush() {
	if s.path == "" {
		return
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	raw, err := json.Marshal(s)
	s.dirty = false
	s.mu.Unlock()
	if err != nil || len(raw) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("stats: mkdir failed: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("stats: write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("stats: rename failed: %v", err)
	}
}

// Close 停止后台落盘并做最后一次 Flush（幂等）。
func (s *Stats) Close() {
	s.once.Do(func() { close(s.stopCh) })
	s.Flush()
}

// load 从落盘文件恢复历史（文件缺失/损坏时静默从零开始，不影响网关主流程）。
func (s *Stats) load() {
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var disk Stats
	if err := json.Unmarshal(raw, &disk); err != nil {
		log.Printf("stats: load parse failed: %v", err)
		return
	}
	s.Total = disk.Total
	if disk.ByModel != nil {
		s.ByModel = disk.ByModel
	}
	if disk.ByUID != nil {
		s.ByUID = disk.ByUID
	}
	if disk.ByRealm != nil {
		s.ByRealm = disk.ByRealm
	}
	if disk.Hourly != nil {
		s.Hourly = disk.Hourly
	}
}

// DimEntry 维度条目（分模型 / 分账号 / 分域）。
type DimEntry struct {
	Name string `json:"name"`
	UsageTotals
}

// HourEntry 按小时趋势条目。
type HourEntry struct {
	Hour string `json:"hour"`
	UsageTotals
}

// StatsSnapshot /stats 返回结构：各维度按 total 降序，趋势按时间升序。
type StatsSnapshot struct {
	Total       UsageTotals `json:"total"`
	ByModel     []DimEntry  `json:"by_model"`
	ByUID       []DimEntry  `json:"by_uid"`
	ByRealm     []DimEntry  `json:"by_realm"`
	Hourly      []HourEntry `json:"hourly"`
	GeneratedAt int64       `json:"generated_at"`
}

// Snapshot 返回当前统计快照（拷贝，调用方不持有锁）。
func (s *Stats) Snapshot() StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := StatsSnapshot{Total: s.Total, GeneratedAt: time.Now().Unix()}
	for name, u := range s.ByModel {
		out.ByModel = append(out.ByModel, DimEntry{Name: name, UsageTotals: u})
	}
	for name, u := range s.ByUID {
		out.ByUID = append(out.ByUID, DimEntry{Name: name, UsageTotals: u})
	}
	for name, u := range s.ByRealm {
		out.ByRealm = append(out.ByRealm, DimEntry{Name: name, UsageTotals: u})
	}
	for h, u := range s.Hourly {
		out.Hourly = append(out.Hourly, HourEntry{Hour: h, UsageTotals: u})
	}
	sort.Slice(out.ByModel, func(i, j int) bool { return out.ByModel[i].Total > out.ByModel[j].Total })
	sort.Slice(out.ByUID, func(i, j int) bool { return out.ByUID[i].Total > out.ByUID[j].Total })
	sort.Slice(out.ByRealm, func(i, j int) bool { return out.ByRealm[i].Total > out.ByRealm[j].Total })
	sort.Slice(out.Hourly, func(i, j int) bool { return out.Hourly[i].Hour < out.Hourly[j].Hour })
	return out
}
