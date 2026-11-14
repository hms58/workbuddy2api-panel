// tasknotify.go 钉钉 / 企业微信群机器人 webhook 通知（本仓定制，上游没有对应实现）。
//
// 为什么全部集中在这一个文件：本项目要持续合并上游主干，通知是旁路能力，
// 逻辑集中在此；既有文件只有 2 处 1 行调用点（growthclaim.go 的 ListALLTasks、
// tasks.go 的 accountTasks）+ 1 行在 taskaudit.go 的领取出口。
//
// 配置（都不配 = 全静默，不影响任何主流程），两处来源，环境变量优先：
//
// 1) 环境变量：WB2A_NOTIFY_DINGTALK_URL / WB2A_NOTIFY_DINGTALK_SECRET / WB2A_NOTIFY_WECOM_URL
// 2) task-logs.json 同目录的 notify.json：
//
//	{
//	  "dingtalk": {"url": "https://oapi.dingtalk.com/robot/send?access_token=...", "secret": "SEC..."},
//	  "wecom":    {"url": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=..."},
//	  "events":   {"new_task": true, "auto_claim": true}
//	}
//
// 事件（当前只这两类）：
//   - 新任务：某账号首次出现新的 task_code（首次见到该账号只记录、不提醒，
//     否则新账号会把整张任务表当「新任务」推一遍）
//   - 自动领取积分：via=auto 的领奖成功且真的拿到分/能；2s 内的多笔合并成一条，
//     避免一轮 29 项把机器人打成刷屏（企业微信限制 20 条/分钟）
package panel

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

const (
	// notifyHTTPTimeout 单次 webhook 请求超时：通知是旁路，宁可丢也不吊住调用方。
	notifyHTTPTimeout = 10 * time.Second
	// 钉钉 markdown 单条 20000 字节、企业微信 4096 字节，这里留出余量。
	notifyDingTalkMaxBytes = 18000
	notifyWeComMaxBytes    = 3800
	// 已见任务码的保存规模：账号数与每账号码数上限（超出按最近更新淘汰）。
	notifySeenMaxAccounts = 200
	notifySeenMaxCodes    = 400
	// notifySeenFile / notifyConfigFile 与 task-logs.json 同目录。
	notifySeenFile   = "task-seen.json"
	notifyConfigFile = "notify.json"
)

// notifyClaimBatchIdle 领取提醒的合并窗口：最后一次领奖后静默这么久才发。
// 用 var 而不是 const：单测要把它缩到毫秒级。
var notifyClaimBatchIdle = 2 * time.Second

// notifyClaimBatchMax 单条消息最多合并多少笔领取，超出立即发一条。
const notifyClaimBatchMax = 20

// notifyTarget 一个推送目标（钉钉或企业微信群机器人）。
type notifyTarget struct {
	kind   string // dingtalk | wecom
	url    string
	secret string // 仅钉钉签名用
}

// notifyEndpoint notify.json 里的一段 webhook 配置。
type notifyEndpoint struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

// notifyFile notify.json 的结构。
type notifyFile struct {
	DingTalk notifyEndpoint `json:"dingtalk"`
	WeCom    notifyEndpoint `json:"wecom"`
	Events   struct {
		NewTask   *bool `json:"new_task"`
		AutoClaim *bool `json:"auto_claim"`
	} `json:"events"`
}

// claimNotice 一笔自动领取（待合并）。
type claimNotice struct {
	uid    string
	label  string // 昵称(uid8)
	code   string
	title  string
	credit int64
	energy int64
}

// seenAccount 某账号见过的任务码（用于判断「新任务」）。
type seenAccount struct {
	Codes     []string  `json:"codes"`
	UpdatedAt time.Time `json:"updated_at"`
}

// seenFile task-seen.json 的结构。
type seenFile struct {
	Version  int                    `json:"version"`
	Accounts map[string]seenAccount `json:"accounts"`
}

// taskNotify 通知器：配置、已见任务码、领取合并缓冲都挂在这里。
// 一个归档目录对应一个实例（见 taskNotifierFor）。
type taskNotify struct {
	dir    string // 状态/配置目录；空 = 纯内存（未配置归档的测试场景）
	client *http.Client

	mu      sync.Mutex
	loaded  bool
	targets []notifyTarget
	newTask bool
	claims  bool

	seen       map[string]seenAccount
	seenLoaded bool

	pending []claimNotice
	timer   *time.Timer
}

// 按目录复用实例：生产只有一个归档目录，测试用 t.TempDir() 各自隔离。
var (
	taskNotifyMu    sync.Mutex
	taskNotifyIndex = map[string]*taskNotify{}
)

func taskNotifierFor(dir string) *taskNotify {
	taskNotifyMu.Lock()
	defer taskNotifyMu.Unlock()
	if n, ok := taskNotifyIndex[dir]; ok {
		return n
	}
	n := &taskNotify{
		dir:    dir,
		client: &http.Client{Timeout: notifyHTTPTimeout},
		seen:   map[string]seenAccount{},
	}
	taskNotifyIndex[dir] = n
	return n
}

// archiveDir 任务归档所在目录（未配置归档时为空）。方法写在本文件里，
// 免得为了拿一个路径去改 ring.go。
func (r *Ring) archiveDir() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.taskArchive == "" {
		return ""
	}
	return filepath.Dir(r.taskArchive)
}

// taskNotify 取当前面板的通知器（按归档目录复用）。
func (p *Panel) taskNotify() *taskNotify {
	if p == nil || p.logs == nil {
		return nil
	}
	return taskNotifierFor(p.logs.archiveDir())
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// reload 读 notify.json，再让环境变量覆盖（env 优先，便于容器里注入）。
func (n *taskNotify) reload() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reloadLocked()
}

func (n *taskNotify) reloadLocked() {
	n.loaded = true
	n.targets = nil
	n.newTask, n.claims = true, true

	f, _ := n.loadFile()
	dingURL := strings.TrimSpace(f.DingTalk.URL)
	dingSecret := strings.TrimSpace(f.DingTalk.Secret)
	wecomURL := strings.TrimSpace(f.WeCom.URL)
	if v := envDingTalkURL(); v != "" {
		dingURL = v
	}
	if v := envDingTalkSecret(); v != "" {
		dingSecret = v
	}
	if v := envWeComURL(); v != "" {
		wecomURL = v
	}
	if dingURL != "" {
		n.targets = append(n.targets, notifyTarget{kind: "dingtalk", url: dingURL, secret: dingSecret})
	}
	if wecomURL != "" {
		n.targets = append(n.targets, notifyTarget{kind: "wecom", url: wecomURL})
	}
	if f.Events.NewTask != nil {
		n.newTask = *f.Events.NewTask
	}
	if f.Events.AutoClaim != nil {
		n.claims = *f.Events.AutoClaim
	}
	if len(n.targets) > 0 {
		kinds := make([]string, 0, len(n.targets))
		for _, t := range n.targets {
			kinds = append(kinds, t.kind)
		}
		log.Printf("[notify] webhook 就绪: %s（新任务=%v 自动领取=%v）", strings.Join(kinds, ","), n.newTask, n.claims)
	}
}

// loadFile 读 notify.json（不存在 = 空配置，不算错误）。返回配置与文件路径。
func (n *taskNotify) loadFile() (notifyFile, string) {
	var f notifyFile
	if n.dir == "" {
		return f, ""
	}
	path := filepath.Join(n.dir, notifyConfigFile)
	raw, err := safefile.Read(path, nil)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [notify] 读取 %s 失败: %v", path, err)
		}
		return f, path
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("WARN: [notify] 解析 %s 失败: %v", path, err)
	}
	return f, path
}

func (n *taskNotify) ensureLoadedLocked() {
	if !n.loaded {
		n.reloadLocked()
	}
}

// enabled 是否配了至少一个可用通道。
func (n *taskNotify) enabled() bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureLoadedLocked()
	return len(n.targets) > 0
}

func (n *taskNotify) events() (newTask, claims bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureLoadedLocked()
	return n.newTask && len(n.targets) > 0, n.claims && len(n.targets) > 0
}

// ---------------------------------------------------------------------------
// 发送
// ---------------------------------------------------------------------------

// send 异步扇出到所有通道（通知失败不影响调用方，只记 WARN）。
func (n *taskNotify) send(title, markdown string) {
	if n == nil || strings.TrimSpace(markdown) == "" {
		return
	}
	n.mu.Lock()
	n.ensureLoadedLocked()
	targets := append([]notifyTarget(nil), n.targets...)
	n.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("WARN: [notify] panic: %v", r)
			}
		}()
		for _, t := range targets {
			if err := n.deliver(t, title, markdown); err != nil {
				log.Printf("WARN: [notify] %s 发送失败: %v", t.kind, err)
			}
		}
	}()
}

// deliver 同步发一条 markdown，返回失败原因（异步路径只记日志，测试接口把它回给前端）。
func (n *taskNotify) deliver(t notifyTarget, title, markdown string) error {
	var payload any
	target := t.url
	switch t.kind {
	case "dingtalk":
		// 钉钉 markdown 单条 20KB：超长截断而不是分段（通知不是报告，截断可接受）。
		target = dingtalkSignedURL(t.url, t.secret, time.Now())
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"title": title, "text": logfmt.Truncate(markdown, notifyDingTalkMaxBytes)},
		}
	default:
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"content": logfmt.Truncate(markdown, notifyWeComMaxBytes)},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("编码失败: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, logfmt.Truncate(string(raw), 200))
	}
	// 钉钉/企业微信都是 {"errcode":0,"errmsg":"ok"}；errcode 非 0 才是真失败。
	var out struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("响应非 JSON: %s", logfmt.Truncate(string(raw), 200))
	}
	if out.ErrCode != 0 {
		return fmt.Errorf("errcode=%d errmsg=%s", out.ErrCode, out.ErrMsg)
	}
	return nil
}

// dingtalkSignedURL 按钉钉自定义机器人规则加签：HMAC-SHA256(secret, ts+"\n"+secret)。
// 未配 secret（关键词/免签模式）时原样返回。
func dingtalkSignedURL(base, secret string, now time.Time) string {
	if secret == "" {
		return base
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String()
}

// ---------------------------------------------------------------------------
// 事件一：自动领取积分
// ---------------------------------------------------------------------------

// notifyAutoClaim 自动领奖成功后的提醒（调用点在 taskaudit.go 的领取出口）。
// 只有 via=auto 且真的拿到分/能才提醒：人工「全部领取」是用户自己点的，不需要回执。
func (p *Panel) notifyAutoClaim(a *auth.Auth, code string, credit, energy int64) {
	n := p.taskNotify()
	if n == nil || (credit <= 0 && energy <= 0) {
		return
	}
	if _, on := n.events(); !on {
		return
	}
	uid, label := "", "-"
	if a != nil {
		uid, label = a.UID, logfmt.Label(a.UID, a.Nickname)
	}
	n.queueClaim(claimNotice{uid: uid, label: label, code: code, title: p.cachedTaskTitle(uid, code), credit: credit, energy: energy})
}

// cachedTaskTitle 从批量任务缓存里取标题（不额外打上游；取不到就只显示任务码）。
func (p *Panel) cachedTaskTitle(uid, code string) string {
	if uid == "" || code == "" {
		return ""
	}
	lists, ok := p.cachedTaskLists(uid)
	if !ok {
		return ""
	}
	for _, t := range lists.cnTaskList {
		if t.TaskCode == code {
			return t.Title
		}
	}
	for _, t := range lists.mpTaskList {
		if t.TaskCode == code {
			return t.Title
		}
	}
	return ""
}

// queueClaim 把一笔领取并入缓冲；窗口内多笔合并成一条消息。
func (n *taskNotify) queueClaim(c claimNotice) {
	n.mu.Lock()
	for i := range n.pending {
		if n.pending[i].uid == c.uid && n.pending[i].code == c.code {
			n.pending[i].credit += c.credit
			n.pending[i].energy += c.energy
			n.mu.Unlock()
			return
		}
	}
	n.pending = append(n.pending, c)
	if len(n.pending) >= notifyClaimBatchMax {
		batch := n.takePendingLocked()
		n.mu.Unlock()
		n.sendClaimBatch(batch)
		return
	}
	if n.timer != nil {
		n.timer.Stop()
	}
	n.timer = time.AfterFunc(notifyClaimBatchIdle, n.flushClaims)
	n.mu.Unlock()
}

func (n *taskNotify) flushClaims() {
	n.mu.Lock()
	batch := n.takePendingLocked()
	n.mu.Unlock()
	n.sendClaimBatch(batch)
}

func (n *taskNotify) takePendingLocked() []claimNotice {
	if n.timer != nil {
		n.timer.Stop()
		n.timer = nil
	}
	batch := n.pending
	n.pending = nil
	return batch
}

func (n *taskNotify) sendClaimBatch(batch []claimNotice) {
	if len(batch) == 0 {
		return
	}
	title, md := claimBatchMarkdown(batch)
	n.send(title, md)
}

// claimBatchMarkdown 领取汇总消息（纯函数，便于单测断言）。
func claimBatchMarkdown(batch []claimNotice) (title, md string) {
	var credit, energy int64
	var b strings.Builder
	b.WriteString("### 自动领取积分\n\n")
	for _, c := range batch {
		credit += c.credit
		energy += c.energy
		name := c.title
		if name == "" {
			name = c.code
		}
		label := name
		if name != c.code {
			label += "（" + c.code + "）"
		}
		b.WriteString("- **" + c.label + "** " + label + gainSuffix(c.credit, c.energy) + "\n")
	}
	b.WriteString("\n合计 **" + gainText(credit, energy) + "**")
	return "自动领取积分", b.String()
}

// gainText "+100 分 +5 能"（都为 0 时返回空串，不带前导空格）。
func gainText(credit, energy int64) string {
	var parts []string
	if credit > 0 {
		parts = append(parts, "+"+strconv.FormatInt(credit, 10)+" 分")
	}
	if energy > 0 {
		parts = append(parts, "+"+strconv.FormatInt(energy, 10)+" 能")
	}
	return strings.Join(parts, " ")
}

// gainSuffix 同 gainText，但非空时带前导空格，便于拼在正文后面。
func gainSuffix(credit, energy int64) string {
	if g := gainText(credit, energy); g != "" {
		return " " + g
	}
	return ""
}

// ---------------------------------------------------------------------------
// 事件二：任务活动有新任务
// ---------------------------------------------------------------------------

// noteTaskList 记录账号见过的任务码；首次出现的新码触发「新任务」提醒。
// 调用点：ListALLTasks（队列/补领清扫）与 accountTasks（面板打开任务弹窗）。
func (p *Panel) noteTaskList(a *auth.Auth, tasks []upstream.Task) {
	n := p.taskNotify()
	if n == nil || a == nil {
		return
	}
	on, _ := n.events()
	if !on {
		return
	}
	codes := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.TaskCode != "" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return
	}
	fresh, firstSight := n.markSeen(a.UID, codes)
	if firstSight || len(fresh) == 0 {
		return
	}
	byCode := make(map[string]upstream.Task, len(tasks))
	for _, t := range tasks {
		byCode[t.TaskCode] = t
	}
	list := make([]upstream.Task, 0, len(fresh))
	for _, code := range fresh {
		if t, ok := byCode[code]; ok {
			list = append(list, t)
		} else {
			list = append(list, upstream.Task{TaskCode: code})
		}
	}
	title, md := newTasksMarkdown(a, list)
	n.send(title, md)
}

// markSeen 合并任务码并返回本次新出现的码；firstSight=true 表示首次记录该账号
// （调用方据此不提醒，避免新账号把整张任务表当新任务推一遍）。
func (n *taskNotify) markSeen(uid string, codes []string) (fresh []string, firstSight bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.loadSeenLocked()
	prev, ok := n.seen[uid]
	known := make(map[string]bool, len(prev.Codes))
	for _, c := range prev.Codes {
		known[c] = true
	}
	for _, c := range codes {
		if !known[c] {
			fresh = append(fresh, c)
			known[c] = true
		}
	}
	merged := append(append([]string{}, prev.Codes...), fresh...)
	if len(merged) > notifySeenMaxCodes {
		merged = merged[len(merged)-notifySeenMaxCodes:]
	}
	n.seen[uid] = seenAccount{Codes: merged, UpdatedAt: time.Now().UTC()}
	n.saveSeenLocked()
	if !ok {
		// 首次见到该账号：整张表都「没见过」，但不是新任务——只记录，不提醒。
		return nil, true
	}
	return fresh, false
}

func (n *taskNotify) loadSeenLocked() {
	if n.seenLoaded || n.dir == "" {
		n.seenLoaded = true
		return
	}
	n.seenLoaded = true
	path := filepath.Join(n.dir, notifySeenFile)
	raw, err := safefile.Read(path, nil)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [notify] 读取 %s 失败: %v", path, err)
		}
		return
	}
	var f seenFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("WARN: [notify] 解析 %s 失败: %v", path, err)
		return
	}
	if f.Accounts != nil {
		n.seen = f.Accounts
	}
}

func (n *taskNotify) saveSeenLocked() {
	if n.dir == "" {
		return
	}
	accounts := n.seen
	if len(accounts) > notifySeenMaxAccounts {
		type kv struct {
			uid string
			at  time.Time
		}
		list := make([]kv, 0, len(accounts))
		for uid, acc := range accounts {
			list = append(list, kv{uid: uid, at: acc.UpdatedAt})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].at.After(list[j].at) })
		trimmed := make(map[string]seenAccount, notifySeenMaxAccounts)
		for _, it := range list[:notifySeenMaxAccounts] {
			trimmed[it.uid] = accounts[it.uid]
		}
		accounts = trimmed
	}
	raw, err := json.Marshal(seenFile{Version: 1, Accounts: accounts})
	if err != nil {
		log.Printf("WARN: [notify] 编码 %s 失败: %v", notifySeenFile, err)
		return
	}
	if err := safefile.WriteKeep(filepath.Join(n.dir, notifySeenFile), raw, 0o600); err != nil {
		log.Printf("WARN: [notify] 保存 %s 失败: %v", notifySeenFile, err)
	}
}

// newTasksMarkdown 新任务提醒消息（纯函数，便于单测断言）。
func newTasksMarkdown(a *auth.Auth, fresh []upstream.Task) (title, md string) {
	label := "-"
	if a != nil {
		label = logfmt.Label(a.UID, a.Nickname)
	}
	var b strings.Builder
	b.WriteString("### 新任务提醒\n\n**" + label + "** 新增 " + strconv.Itoa(len(fresh)) + " 个任务\n\n")
	for _, t := range fresh {
		name := t.Title
		if name == "" {
			name = t.TaskCode
		}
		line := "- " + name
		if t.TaskCode != "" && t.TaskCode != name {
			line += "（" + t.TaskCode + "）"
		}
		line += gainSuffix(t.Credit, t.Energy)
		if end := taskEndLabel(t.ValidEnd); end != "" {
			line += " · 截止 " + end
		}
		b.WriteString(line + "\n")
	}
	return "新任务提醒", b.String()
}

// taskEndLabel 上游 valid_end 的展示文本（文本形态截到分钟，epoch/RFC3339 转本地时间）。
// 解析不出来返回空串（不显示「截止」），与前端 taskEndText 的口径一致。
func taskEndLabel(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// 无时区的文本形态（"2026-11-20 23:59:59" / "2026-11-20T23:59:00"）直接截到分钟；
	// 带 Z/偏移的必须走 RFC3339 换算成本地时间，否则会显示成 UTC 时刻。
	rest := ""
	if len(s) > 19 {
		rest = s[19:]
	}
	if len(s) >= 16 && (s[10] == ' ' || s[10] == 'T') && !strings.ContainsAny(rest, "Zz+-") {
		return s[:10] + " " + s[11:16]
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		t := time.Unix(n, 0)
		if n >= 1e12 {
			t = time.UnixMilli(n)
		}
		return t.In(cst.Loc).Format("2006-01-02 15:04")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(cst.Loc).Format("2006-01-02 15:04")
	}
	return ""
}

// 环境变量读取（集中一处：reload 与面板视图都要用同一份口径）。
func envDingTalkURL() string    { return strings.TrimSpace(os.Getenv("WB2A_NOTIFY_DINGTALK_URL")) }
func envDingTalkSecret() string { return strings.TrimSpace(os.Getenv("WB2A_NOTIFY_DINGTALK_SECRET")) }
func envWeComURL() string       { return strings.TrimSpace(os.Getenv("WB2A_NOTIFY_WECOM_URL")) }
