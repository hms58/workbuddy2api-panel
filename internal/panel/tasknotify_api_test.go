package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// newNotifyPanel 造一个归档目录在 dir 的面板（notify.json / task-seen.json 都落这里）。
func newNotifyPanel(t *testing.T, dir string) *Panel {
	t.Helper()
	p := New(Config{})
	r := NewRing(10)
	if err := r.SetTaskArchive(filepath.Join(dir, "task-logs.json")); err != nil {
		t.Fatal(err)
	}
	p.logs = r
	return p
}

func notifyCall(t *testing.T, p *Panel, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req = httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func decodeNotifyView(t *testing.T, rec *httptest.ResponseRecorder) notifyView {
	t.Helper()
	var v notifyView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return v
}

// 保存后能读回：URL 打码（只留尾 4 位）、密钥只报「已配置」、事件开关生效。
func TestNotifySaveAndView(t *testing.T) {
	dir := t.TempDir()
	p := newNotifyPanel(t, dir)
	const token = "abcdefgh1234"
	body := `{"dingtalk":{"url":"https://oapi.dingtalk.com/robot/send?access_token=` + token + `","secret":"SECx"},` +
		`"wecom":{"url":""},"events":{"new_task":false,"auto_claim":true}}`
	if rec := notifyCall(t, p, "POST", "/panel/api/notify", body); rec.Code != 200 {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	// 文件确实落在归档目录
	raw, err := os.ReadFile(filepath.Join(dir, notifyConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), token) {
		t.Fatalf("notify.json 未写入 token: %s", raw)
	}

	v := decodeNotifyView(t, notifyCall(t, p, "GET", "/panel/api/notify", ""))
	if !v.Enabled || !v.DingTalk.Configured || !v.DingTalk.SecretSet {
		t.Fatalf("view = %+v", v)
	}
	if strings.Contains(v.DingTalk.URL, token) || !strings.HasSuffix(v.DingTalk.URL, "1234") {
		t.Fatalf("URL 未打码: %s", v.DingTalk.URL)
	}
	if v.Events.NewTask || !v.Events.AutoClaim {
		t.Fatalf("events = %+v", v.Events)
	}
	if v.WeCom.Configured {
		t.Fatalf("企业微信不应被配置: %+v", v.WeCom)
	}
	if v.Path == "" || v.Source == "" {
		t.Fatalf("path/source 缺失: %+v", v)
	}
}

// 留空 = 不修改；clear_* 才清除；非法 URL 直接 400 且不落盘。
func TestNotifySaveSemantics(t *testing.T) {
	dir := t.TempDir()
	p := newNotifyPanel(t, dir)
	seed := `{"dingtalk":{"url":"https://oapi.dingtalk.com/robot/send?access_token=keepme","secret":"SECx"}}`
	if rec := notifyCall(t, p, "POST", "/panel/api/notify", seed); rec.Code != 200 {
		t.Fatalf("seed = %d %s", rec.Code, rec.Body.String())
	}

	// 只改事件：URL/密钥必须原样保留
	if rec := notifyCall(t, p, "POST", "/panel/api/notify", `{"events":{"new_task":true}}`); rec.Code != 200 {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeNotifyView(t, notifyCall(t, p, "GET", "/panel/api/notify", ""))
	if !v.DingTalk.Configured || !v.DingTalk.SecretSet || !v.Events.NewTask {
		t.Fatalf("留空覆盖了已有值: %+v", v)
	}

	// 企业微信缺 key → 400，且不写入
	if rec := notifyCall(t, p, "POST", "/panel/api/notify", `{"wecom":{"url":"https://example.com/send"}}`); rec.Code != 400 {
		t.Fatalf("bad url code = %d %s", rec.Code, rec.Body.String())
	}
	if v := decodeNotifyView(t, notifyCall(t, p, "GET", "/panel/api/notify", "")); v.WeCom.Configured {
		t.Fatal("非法地址不应落盘")
	}

	// 清除钉钉通道
	if rec := notifyCall(t, p, "POST", "/panel/api/notify", `{"clear_dingtalk":true}`); rec.Code != 200 {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeNotifyView(t, notifyCall(t, p, "GET", "/panel/api/notify", ""))
	if v.DingTalk.Configured || v.DingTalk.SecretSet || v.Enabled {
		t.Fatalf("清除后仍有残留: %+v", v)
	}
}

// 「发送测试消息」必须真发到 webhook，并把失败原因回给前端。
func TestNotifyTestEndpoint(t *testing.T) {
	rec := &hookRecorder{}
	srv := rec.server(t)
	dir := t.TempDir()
	cfg := `{"dingtalk":{"url":"` + srv.URL + `/robot/send?access_token=t"}}`
	if err := os.WriteFile(filepath.Join(dir, notifyConfigFile), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newNotifyPanel(t, dir)
	res := notifyCall(t, p, "POST", "/panel/api/notify/test", "")
	if res.Code != 200 {
		t.Fatalf("test = %d %s", res.Code, res.Body.String())
	}
	var out struct {
		OK   bool     `json:"ok"`
		Sent []string `json:"sent"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || len(out.Sent) != 1 || out.Sent[0] != "dingtalk" {
		t.Fatalf("out = %+v", out)
	}
	if rec.count() != 1 {
		t.Fatalf("webhook 收到 %d 次，期望 1", rec.count())
	}
	body, _ := rec.last()
	md, _ := body["markdown"].(map[string]any)
	if !strings.Contains(md["text"].(string), "通知自检") {
		t.Fatalf("测试消息内容 = %v", md["text"])
	}

	// 未配置通道 → 400 且提示清楚
	empty := newNotifyPanel(t, t.TempDir())
	if r := notifyCall(t, empty, "POST", "/panel/api/notify/test", ""); r.Code != 400 {
		t.Fatalf("空配置 test = %d %s", r.Code, r.Body.String())
	}
}

// 端到端：面板侧钩子（noteTaskList / notifyAutoClaim）→ 合并窗口 → 真发到 webhook。
func TestNotifyEventHooks(t *testing.T) {
	rec := &hookRecorder{}
	srv := rec.server(t)
	dir := t.TempDir()
	cfg := `{"dingtalk":{"url":"` + srv.URL + `/robot/send?access_token=t"},"events":{"new_task":true,"auto_claim":true}}`
	if err := os.WriteFile(filepath.Join(dir, notifyConfigFile), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	oldIdle := notifyClaimBatchIdle
	notifyClaimBatchIdle = 20 * time.Millisecond
	t.Cleanup(func() { notifyClaimBatchIdle = oldIdle })

	p := newNotifyPanel(t, dir)
	a := &auth.Auth{UID: "u1", Nickname: "甲"}
	first := []upstream.Task{{TaskCode: "chat_5", Title: "对话5次", Credit: 100}}
	p.noteTaskList(a, first) // 首次见到该账号：只记录，不提醒
	if rec.count() != 0 {
		t.Fatalf("首次不应推送，收到 %d 条", rec.count())
	}
	p.noteTaskList(a, append(first, upstream.Task{TaskCode: "black_cat", Title: "夜猫子", Energy: 5}))
	waitHooks(t, rec, 1)
	body, _ := rec.last()
	md, _ := body["markdown"].(map[string]any)
	if text := md["text"].(string); !strings.Contains(text, "black_cat") || !strings.Contains(text, "夜猫子") {
		t.Fatalf("新任务消息 = %s", text)
	}

	// 自动领取：走同一个合并窗口
	p.notifyAutoClaim(a, "chat_5", 100, 5)
	waitHooks(t, rec, 2)
	body, _ = rec.last()
	md, _ = body["markdown"].(map[string]any)
	if text := md["text"].(string); !strings.Contains(text, "+100 分 +5 能") {
		t.Fatalf("领取消息 = %s", text)
	}
}

func waitHooks(t *testing.T, rec *hookRecorder, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rec.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("webhook 收到 %d 条，期望 %d", rec.count(), want)
}

// 每渠道开关：关掉后即使 URL 还在也不推送（URL/密钥保留，重新打开即恢复）。
func TestNotifyChannelSwitch(t *testing.T) {
	dir := t.TempDir()
	p := newNotifyPanel(t, dir)
	body := `{"dingtalk":{"enabled":false,"url":"https://oapi.dingtalk.com/robot/send?access_token=t1","secret":"SECx"}}`
	if rec := notifyCall(t, p, "POST", "/panel/api/notify", body); rec.Code != 200 {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeNotifyView(t, notifyCall(t, p, "GET", "/panel/api/notify", ""))
	if v.DingTalk.Enabled {
		t.Fatal("enabled=false 未生效")
	}
	if !v.DingTalk.Configured || v.Enabled {
		t.Fatalf("关掉后应保留配置但不启用: %+v", v)
	}
	if p.taskNotify().enabled() {
		t.Fatal("关掉的渠道仍被当作可用目标")
	}
	if rec := notifyCall(t, p, "POST", "/panel/api/notify/test", ""); rec.Code != 400 {
		t.Fatalf("关掉后测试发送应 400，得到 %d %s", rec.Code, rec.Body.String())
	}

	// 重新打开：URL/密钥仍在，直接可用
	if rec := notifyCall(t, p, "POST", "/panel/api/notify", `{"dingtalk":{"enabled":true}}`); rec.Code != 200 {
		t.Fatalf("re-enable = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeNotifyView(t, notifyCall(t, p, "GET", "/panel/api/notify", ""))
	if !v.DingTalk.Enabled || !v.DingTalk.SecretSet || !v.Enabled {
		t.Fatalf("重新打开后配置丢失: %+v", v)
	}
	if !p.taskNotify().enabled() {
		t.Fatal("重新打开后仍无可用目标")
	}
}

// 三个通知接口都必须过鉴权层（未带 key 一律 401）。
func TestNotifyEndpointsRequireAuth(t *testing.T) {
	p := New(Config{APIKey: "test-key"})
	for _, c := range []struct{ method, path string }{
		{"GET", "/panel/api/notify"},
		{"POST", "/panel/api/notify"},
		{"POST", "/panel/api/notify/test"},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != 401 {
			t.Errorf("%s %s = %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

// 打码只保留末 4 位，其余参数原样保留。
func TestMaskWebhook(t *testing.T) {
	got := maskWebhook("https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=0123456789abcdef")
	if strings.Contains(got, "0123456789abcdef") || !strings.Contains(got, "cdef") {
		t.Fatalf("masked = %s", got)
	}
	if !strings.HasPrefix(got, "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=") {
		t.Fatalf("路径被改动: %s", got)
	}
	if got := maskWebhook(""); got != "" {
		t.Fatalf("空值 = %q", got)
	}
}
