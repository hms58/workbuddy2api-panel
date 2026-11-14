// tasknotify_api.go 通知配置的面板接口（GET/POST /panel/api/notify + 测试发送）。
//
// 与 tasknotify.go 的分工：那边是发送与状态，这边只是「读写 notify.json 的薄壳」。
// 刻意不走 config.json / /panel/api/config：通知是旁路能力，独立文件可以让这块
// 与上游主干完全解耦（panel.go 只多 3 行路由）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
)

// notifyChannelView 单个通道的对外视图（URL 打码，密钥只报「已配置」）。
type notifyChannelView struct {
	Enabled    bool   `json:"enabled"` // 开关：关掉后即使填了 URL 也不推送
	Configured bool   `json:"configured"`
	URL        string `json:"url"`
	SecretSet  bool   `json:"secret_set"`
	Source     string `json:"source"` // file | env | ""
}

// notifyView GET /panel/api/notify 的响应。
type notifyView struct {
	Path     string            `json:"path"`
	Enabled  bool              `json:"enabled"`
	Source   string            `json:"source"` // 配置来自哪个文件（未配置归档时为空）
	DingTalk notifyChannelView `json:"dingtalk"`
	WeCom    notifyChannelView `json:"wecom"`
	Events   struct {
		NewTask   bool `json:"new_task"`
		AutoClaim bool `json:"auto_claim"`
	} `json:"events"`
	EnvManaged struct {
		DingTalk bool `json:"dingtalk"`
		WeCom    bool `json:"wecom"`
	} `json:"env_managed"`
}

// notifySaveRequest POST /panel/api/notify 的请求体。
// url / secret 留空 = 不修改（沿用配置页「留空即不改」口径）；要清空用 clear_*。
type notifySaveRequest struct {
	DingTalk struct {
		Enabled *bool  `json:"enabled"`
		URL     string `json:"url"`
		Secret  string `json:"secret"`
	} `json:"dingtalk"`
	WeCom struct {
		Enabled *bool  `json:"enabled"`
		URL     string `json:"url"`
	} `json:"wecom"`
	Events struct {
		NewTask   *bool `json:"new_task"`
		AutoClaim *bool `json:"auto_claim"`
	} `json:"events"`
	ClearDingTalk bool `json:"clear_dingtalk"`
	ClearWeCom    bool `json:"clear_wecom"`
}

// getNotifyConfig 读取当前通知配置（密钥不外泄）。
func (p *Panel) getNotifyConfig(w http.ResponseWriter, r *http.Request) {
	n := p.taskNotify()
	if n == nil {
		writeErr(w, http.StatusServiceUnavailable, "notify not available")
		return
	}
	writeJSON(w, http.StatusOK, n.view())
}

// saveNotifyConfig 写 notify.json 并热生效（reload 后立即按新配置推送）。
func (p *Panel) saveNotifyConfig(w http.ResponseWriter, r *http.Request) {
	n := p.taskNotify()
	if n == nil {
		writeErr(w, http.StatusServiceUnavailable, "notify not available")
		return
	}
	var req notifySaveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if err := n.apply(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	view := n.view()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notify": view, "restart_required": []string{}})
}

// testNotify 立刻发一条自检消息（前端「发送测试消息」按钮）。
func (p *Panel) testNotify(w http.ResponseWriter, r *http.Request) {
	n := p.taskNotify()
	if n == nil {
		writeErr(w, http.StatusServiceUnavailable, "notify not available")
		return
	}
	sent, failures := n.selfTest()
	if len(failures) > 0 {
		writeErr(w, http.StatusBadGateway, strings.Join(failures, "；"))
		return
	}
	if len(sent) == 0 {
		writeErr(w, http.StatusBadRequest, "尚未配置任何 webhook")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sent": sent})
}

// view 组装对外视图：文件值 + 环境变量覆盖，URL 只露主机与参数名、值打码。
func (n *taskNotify) view() notifyView {
	f, path := n.loadFile()
	var v notifyView
	v.Path, v.Source = path, path
	v.Events.NewTask, v.Events.AutoClaim = true, true
	if f.Events.NewTask != nil {
		v.Events.NewTask = *f.Events.NewTask
	}
	if f.Events.AutoClaim != nil {
		v.Events.AutoClaim = *f.Events.AutoClaim
	}
	dingURL, dingSecret, wecomURL := strings.TrimSpace(f.DingTalk.URL), strings.TrimSpace(f.DingTalk.Secret), strings.TrimSpace(f.WeCom.URL)
	v.DingTalk = notifyChannelView{Enabled: f.DingTalk.active(), Configured: dingURL != "", URL: maskWebhook(dingURL), SecretSet: dingSecret != "", Source: "file"}
	v.WeCom = notifyChannelView{Enabled: f.WeCom.active(), Configured: wecomURL != "", URL: maskWebhook(wecomURL), Source: "file"}
	if dingURL == "" && dingSecret == "" {
		v.DingTalk.Source = ""
	}
	if wecomURL == "" {
		v.WeCom.Source = ""
	}
	// 环境变量优先：托管时前端置灰（改了也不会生效），且视同启用。
	if env := strings.TrimSpace(envDingTalkURL()); env != "" {
		v.DingTalk = notifyChannelView{Enabled: true, Configured: true, URL: maskWebhook(env), SecretSet: envDingTalkSecret() != "", Source: "env"}
		v.EnvManaged.DingTalk = true
	}
	if env := strings.TrimSpace(envWeComURL()); env != "" {
		v.WeCom = notifyChannelView{Enabled: true, Configured: true, URL: maskWebhook(env), Source: "env"}
		v.EnvManaged.WeCom = true
	}
	v.Enabled = (v.DingTalk.Configured && v.DingTalk.Enabled) || (v.WeCom.Configured && v.WeCom.Enabled)
	return v
}

// apply 把请求合并进 notify.json（留空不改、clear_* 清空），随后 reload 热生效。
func (n *taskNotify) apply(req notifySaveRequest) error {
	f, path := n.loadFile()
	if path == "" {
		return errNoArchiveDir
	}
	dingURL := strings.TrimSpace(req.DingTalk.URL)
	dingSecret := strings.TrimSpace(req.DingTalk.Secret)
	wecomURL := strings.TrimSpace(req.WeCom.URL)
	if dingURL != "" {
		if err := validateWebhookURL("dingtalk", dingURL); err != nil {
			return err
		}
	}
	if wecomURL != "" {
		if err := validateWebhookURL("wecom", wecomURL); err != nil {
			return err
		}
	}
	if req.ClearDingTalk {
		f.DingTalk = notifyEndpoint{}
	} else {
		if req.DingTalk.Enabled != nil {
			v := *req.DingTalk.Enabled
			f.DingTalk.Enabled = &v
		}
		if dingURL != "" {
			f.DingTalk.URL = dingURL
		}
		if dingSecret != "" {
			f.DingTalk.Secret = dingSecret
		}
	}
	if req.ClearWeCom {
		f.WeCom = notifyEndpoint{}
	} else {
		if req.WeCom.Enabled != nil {
			v := *req.WeCom.Enabled
			f.WeCom.Enabled = &v
		}
		if wecomURL != "" {
			f.WeCom.URL = wecomURL
		}
	}
	if req.Events.NewTask != nil {
		v := *req.Events.NewTask
		f.Events.NewTask = &v
	}
	if req.Events.AutoClaim != nil {
		v := *req.Events.AutoClaim
		f.Events.AutoClaim = &v
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := safefile.WriteKeep(path, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	n.reload()
	return nil
}

// selfTest 同步发一条自检消息，返回成功的通道与失败原因。
func (n *taskNotify) selfTest() (sent []string, failures []string) {
	n.mu.Lock()
	n.ensureLoadedLocked()
	targets := append([]notifyTarget(nil), n.targets...)
	n.mu.Unlock()
	const md = "### 通知自检\n\n**workbuddy2api** 面板通知已接通，任务提醒将推送到这里。\n\n> 本条为手动触发的测试消息，可忽略。"
	for _, t := range targets {
		if err := n.deliver(t, "通知自检", md); err != nil {
			failures = append(failures, t.kind+": "+err.Error())
			continue
		}
		sent = append(sent, t.kind)
	}
	return sent, failures
}

// errNoArchiveDir 没有归档目录时无法落盘（面板未注入 task-logs.json 路径）。
var errNoArchiveDir = errNotifyNoDir{}

type errNotifyNoDir struct{}

func (errNotifyNoDir) Error() string {
	return "未找到任务归档目录，无法保存通知配置（检查 state_file 配置）"
}

// validateWebhookURL 只做「能不能用」的最小校验：http(s) + 各自必需的 query 参数。
func validateWebhookURL(kind, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errNotifyBadURL("地址无法解析: " + err.Error())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errNotifyBadURL("需要 http(s) 地址")
	}
	q := u.Query()
	switch kind {
	case "dingtalk":
		if q.Get("access_token") == "" {
			return errNotifyBadURL("钉钉地址缺少 access_token 参数")
		}
	case "wecom":
		if q.Get("key") == "" {
			return errNotifyBadURL("企业微信地址缺少 key 参数")
		}
	}
	return nil
}

type errNotifyBadURL string

func (e errNotifyBadURL) Error() string { return string(e) }

// maskWebhook 保留地址结构，把 access_token / key 的值换成 ****尾4位。
// 前端要能看出「配了哪个机器人」，但不能把凭据回显到页面/浏览器历史里。
func maskWebhook(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "****"
	}
	q := u.Query()
	for _, k := range []string{"access_token", "key"} {
		if v := q.Get(k); v != "" {
			q.Set(k, maskSecret(v))
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// maskSecret 只留末 4 位。
func maskSecret(v string) string {
	if len(v) <= 4 {
		return "****"
	}
	return "****" + v[len(v)-4:]
}
