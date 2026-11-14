package panel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// hookRecorder 记录收到的 webhook 请求（body + query），用于断言报文形态。
type hookRecorder struct {
	mu      sync.Mutex
	bodies  []map[string]any
	queries []url.Values
	paths   []string
}

func (h *hookRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		h.mu.Lock()
		h.bodies = append(h.bodies, body)
		h.queries = append(h.queries, r.URL.Query())
		h.paths = append(h.paths, r.URL.Path)
		h.mu.Unlock()
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (h *hookRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies)
}

func (h *hookRecorder) last() (map[string]any, url.Values) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bodies) == 0 {
		return nil, nil
	}
	return h.bodies[len(h.bodies)-1], h.queries[len(h.queries)-1]
}

// newTestNotifier 直接构造通知器（绕过 notify.json/env），target 指向测试服务器。
func newTestNotifier(srv *httptest.Server, kind, path, secret string) *taskNotify {
	n := &taskNotify{dir: "", client: srv.Client(), seen: map[string]seenAccount{}}
	n.loaded = true
	n.targets = []notifyTarget{{kind: kind, url: srv.URL + path, secret: secret}}
	n.newTask, n.claims = true, true
	return n
}

// 钉钉加签口径：HMAC-SHA256(secret, ts+"\n"+secret) → base64 → query 里 URL 编码。
func TestDingTalkSignedURL(t *testing.T) {
	base := "https://oapi.dingtalk.com/robot/send?access_token=tk"
	now := time.UnixMilli(1700000000000)
	signed := dingtalkSignedURL(base, "SECxyz", now)
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("access_token") != "tk" {
		t.Fatalf("access_token lost: %s", signed)
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Fatalf("timestamp = %q", q.Get("timestamp"))
	}
	mac := hmac.New(sha256.New, []byte("SECxyz"))
	mac.Write([]byte("1700000000000\nSECxyz"))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); q.Get("sign") != want {
		t.Fatalf("sign = %q want %q", q.Get("sign"), want)
	}
	// 未配 secret（关键词/免签模式）必须原样返回，不能凭空加参数。
	if got := dingtalkSignedURL(base, "", now); got != base {
		t.Fatalf("unsigned url = %s", got)
	}
}

// 钉钉报文：msgtype=markdown，带 title；签名参数进 query 而不是 body。
func TestNotifyDeliverDingTalk(t *testing.T) {
	rec := &hookRecorder{}
	srv := rec.server(t)
	n := newTestNotifier(srv, "dingtalk", "/robot/send?access_token=tk", "SECxyz")
	n.deliver(n.targets[0], "新任务提醒", "### 新任务提醒\n\n- 对话5次（chat_5）+100 分")

	if rec.count() != 1 {
		t.Fatalf("requests = %d want 1", rec.count())
	}
	body, query := rec.last()
	if body["msgtype"] != "markdown" {
		t.Fatalf("msgtype = %v", body["msgtype"])
	}
	md, _ := body["markdown"].(map[string]any)
	if md["title"] != "新任务提醒" {
		t.Fatalf("title = %v", md["title"])
	}
	if !strings.Contains(md["text"].(string), "chat_5") {
		t.Fatalf("text = %v", md["text"])
	}
	if query.Get("sign") == "" || query.Get("timestamp") == "" {
		t.Fatalf("钉钉必须带签名参数: %v", query)
	}
}

// 企业微信报文：msgtype=markdown，内容在 markdown.content，且不带钉钉的签名参数。
func TestNotifyDeliverWeCom(t *testing.T) {
	rec := &hookRecorder{}
	srv := rec.server(t)
	n := newTestNotifier(srv, "wecom", "/cgi-bin/webhook/send?key=k", "")
	n.deliver(n.targets[0], "自动领取积分", "### 自动领取积分\n\n合计 **+100 分**")

	if rec.count() != 1 {
		t.Fatalf("requests = %d want 1", rec.count())
	}
	body, query := rec.last()
	if body["msgtype"] != "markdown" {
		t.Fatalf("msgtype = %v", body["msgtype"])
	}
	md, _ := body["markdown"].(map[string]any)
	if !strings.Contains(md["content"].(string), "合计") {
		t.Fatalf("content = %v", md["content"])
	}
	if query.Get("sign") != "" {
		t.Fatalf("企业微信不应带 sign: %v", query)
	}
}

// 领取提醒必须合并：一轮 29 项逐条发会被企业微信限流（20 条/分钟），
// 同账号同任务码的重复领取也要合并成一笔。
func TestNotifyClaimBatchMerged(t *testing.T) {
	oldIdle := notifyClaimBatchIdle
	notifyClaimBatchIdle = 20 * time.Millisecond
	t.Cleanup(func() { notifyClaimBatchIdle = oldIdle })

	rec := &hookRecorder{}
	srv := rec.server(t)
	n := newTestNotifier(srv, "wecom", "/hook", "")

	n.queueClaim(claimNotice{uid: "u1", label: "甲(u1)", code: "chat_5", title: "对话5次", credit: 100})
	n.queueClaim(claimNotice{uid: "u1", label: "甲(u1)", code: "chat_5", title: "对话5次", credit: 50})
	n.queueClaim(claimNotice{uid: "u1", label: "甲(u1)", code: "black_cat", title: "夜猫子", energy: 5})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rec.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if rec.count() != 1 {
		t.Fatalf("requests = %d want 1（合并后只发一条）", rec.count())
	}
	body, _ := rec.last()
	md, _ := body["markdown"].(map[string]any)
	content := md["content"].(string)
	if !strings.Contains(content, "chat_5") || !strings.Contains(content, "black_cat") {
		t.Fatalf("content 缺任务: %s", content)
	}
	// chat_5 两笔（100+50）合并成 +150 分，夜猫子 +5 能。
	if !strings.Contains(content, "+150 分") || !strings.Contains(content, "+5 能") {
		t.Fatalf("金额未合并: %s", content)
	}
}

// 新任务判定：首次见到账号只记录不提醒；之后新出现的码才算「新任务」并落盘。
func TestNotifyMarkSeen(t *testing.T) {
	dir := t.TempDir()
	n := taskNotifierFor(dir)
	n.mu.Lock()
	n.seen = map[string]seenAccount{}
	n.seenLoaded = true
	n.mu.Unlock()

	fresh, first := n.markSeen("u1", []string{"chat_5", "black_cat"})
	if !first {
		t.Fatal("首次见到账号必须返回 firstSight=true（调用方据此不提醒）")
	}
	if len(fresh) != 0 {
		t.Fatalf("首次不应产生新任务: %v", fresh)
	}

	fresh, first = n.markSeen("u1", []string{"chat_5", "black_cat", "new_task"})
	if first || len(fresh) != 1 || fresh[0] != "new_task" {
		t.Fatalf("fresh=%v first=%v", fresh, first)
	}

	// 再跑一遍同样的列表不能重复提醒。
	if fresh, _ := n.markSeen("u1", []string{"chat_5", "black_cat", "new_task"}); len(fresh) != 0 {
		t.Fatalf("重复列表又提醒了: %v", fresh)
	}

	// 已见任务码要落盘（重启后不重复提醒）。
	raw, err := os.ReadFile(filepath.Join(dir, notifySeenFile))
	if err != nil {
		t.Fatal(err)
	}
	var f seenFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if got := len(f.Accounts["u1"].Codes); got != 3 {
		t.Fatalf("落盘码数 = %d want 3", got)
	}
}

// 新任务提醒报文：标题/任务码/奖励/截止时间都要出现。
func TestNewTasksMarkdown(t *testing.T) {
	a := &auth.Auth{UID: "u1", Nickname: "甲"}
	title, md := newTasksMarkdown(a, []upstream.Task{
		{TaskCode: "chat_5", Title: "对话5次", Credit: 100, Energy: 5, ValidEnd: "2026-11-20 23:59:59"},
		{TaskCode: "no_title"},
	})
	if title != "新任务提醒" {
		t.Fatalf("title = %q", title)
	}
	for _, want := range []string{"甲(u1)", "对话5次（chat_5）", "+100 分 +5 能", "截止 2026-11-20 23:59", "- no_title"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown 缺 %q:\n%s", want, md)
		}
	}
}

// 领取汇总报文：无标题时退回任务码，金额为 0 时不出现多余空格。
func TestClaimBatchMarkdown(t *testing.T) {
	title, md := claimBatchMarkdown([]claimNotice{
		{label: "甲(u1)", code: "chat_5", credit: 100, energy: 5},
		{label: "乙(u2)", code: "skill_1", title: "技能", credit: 50},
	})
	if title != "自动领取积分" {
		t.Fatalf("title = %q", title)
	}
	for _, want := range []string{"**甲(u1)** chat_5 +100 分 +5 能", "**乙(u2)** 技能（skill_1） +50 分", "合计 **+150 分 +5 能**"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown 缺 %q:\n%s", want, md)
		}
	}
}

// 截止时间三种上游形态：文本截到分钟、epoch 秒/毫秒、RFC3339；非法值不显示。
func TestTaskEndLabel(t *testing.T) {
	cases := map[string]string{
		"2026-11-20 23:59:59":  "2026-11-20 23:59",
		"2026-11-20T23:59:00":  "2026-11-20 23:59",
		"2026-11-20T15:59:00Z": "2026-11-20 23:59",
		"":                     "",
		"not-a-time":           "",
	}
	for in, want := range cases {
		if got := taskEndLabel(in); got != want {
			t.Errorf("taskEndLabel(%q) = %q want %q", in, got, want)
		}
	}
}

// 配置来源：notify.json 与同名目录下的环境变量都能装配出目标；env 优先。
func TestTaskNotifyConfigSources(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"dingtalk":{"url":"https://oapi.dingtalk.com/robot/send?access_token=fromfile","secret":"SEC1"},"events":{"new_task":false}}`
	if err := os.WriteFile(filepath.Join(dir, notifyConfigFile), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	n := taskNotifierFor(dir)
	n.mu.Lock()
	n.loaded = false
	n.mu.Unlock()
	n.reload()
	n.mu.Lock()
	targets := append([]notifyTarget(nil), n.targets...)
	newTask, claims := n.newTask, n.claims
	n.mu.Unlock()
	if len(targets) != 1 || targets[0].kind != "dingtalk" || !strings.Contains(targets[0].url, "fromfile") {
		t.Fatalf("targets = %v", targets)
	}
	if newTask || !claims {
		t.Fatalf("events: newTask=%v claims=%v（notify.json 关了 new_task）", newTask, claims)
	}

	// 环境变量优先于文件。
	t.Setenv("WB2A_NOTIFY_DINGTALK_URL", "https://oapi.dingtalk.com/robot/send?access_token=fromenv")
	t.Setenv("WB2A_NOTIFY_WECOM_URL", "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=k")
	n.reload()
	n.mu.Lock()
	targets = append([]notifyTarget(nil), n.targets...)
	n.mu.Unlock()
	if len(targets) != 2 {
		t.Fatalf("targets = %v want dingtalk+wecom", targets)
	}
	if !strings.Contains(targets[0].url, "fromenv") {
		t.Fatalf("env 未覆盖文件: %v", targets[0])
	}
}

// 未配置任何通道时必须是纯空转：不发请求、不报错（通知是旁路）。
func TestTaskNotifyDisabledIsNoop(t *testing.T) {
	dir := t.TempDir()
	n := taskNotifierFor(dir)
	n.mu.Lock()
	n.loaded = false
	n.mu.Unlock()
	n.reload()
	if n.enabled() {
		t.Fatal("没有配置时不应 enabled")
	}
	n.send("x", "y") // 不应 panic、不应发请求
	n.queueClaim(claimNotice{uid: "u1", code: "chat_5", credit: 100})
}
