// modelgroups.go 面板「模型组」菜单的读写接口。
//
// 独立性：本文件只依赖 modelgroup 包与三个注入回调，不碰 config 结构、
// 不碰既有配置页表单 —— 新菜单对既有功能零影响。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/modelgroup"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// modelGroupsGet 返回组配置 + 每成员运行态。
//
// ⭐ 状态字段取自 pool.ModelLockView()，与账号池「模型锁池」表**同源**（设计文档 §13.5）。
// 不读 modelgroup.Cache：Cache 是带 TTL 的乐观快照，读它会让两块面板给出不同结论。
// 只有 probed_at（上次探活时刻）来自 Cache —— 那是它独有的字段，且不影响状态判定。
func (p *Panel) modelGroupsGet(w http.ResponseWriter, r *http.Request) {
	// 「未装配」与「已装配但一个组都没有」必须区分：
	// 后者是全新部署的合法初始状态 —— 用户要能在面板上新建第一个组。
	// 只有 GroupSet 回调本身没注入（能力未接线）时才返回 501。
	if p.cfg.GroupSet == nil {
		writeErr(w, http.StatusNotImplemented, "model groups not configured")
		return
	}
	gs := p.groupSet() // 可能为 nil：编译后无组时是空集合语义
	now := time.Now()

	// 一次取全量锁行，按 (realm, bare) 建索引：无命中 = 该模型没被锁 = 可用。
	lockIdx := make(map[string]pool.ModelLockRow)
	if p.cfg.Pool != nil {
		for _, row := range p.cfg.Pool.ModelLockView() {
			lockIdx[row.Realm+"\x1f"+row.Model] = row
		}
	}
	cache := p.groupCache() // 只用于 probed_at

	// 无组时 gs 为 nil（compileModelGroups 的空集合语义），此时组列表为空、
	// options 必须回落到「配置里的全局参数」而不是裸零值。
	// 裸 modelgroup.Options{} 的两个 bool 开关零值都是 false，会把
	// prefer_last_ok / sniff_enabled 显示成关闭 —— 与设计默认（都是 true）不符，
	// 且用户一旦保存就会把 false 写进配置，从此再也回不到默认。
	var all []modelgroup.Group
	var opts modelgroup.Options
	if gs != nil {
		all = gs.All()
		opts = gs.Options()
	} else if p.cfg.Live != nil {
		// 走 live 快照：面板保存配置后快照会被替换，读静态字段会拿到过期值。
		opts = p.cfg.Live.Load().ModelGroupOpts
	} else if p.cfg.ModelGroupOpts != nil {
		opts = *p.cfg.ModelGroupOpts
	} else {
		// 兜底：没有配置来源时按设计默认（两个开关都 true）+ 时长缺省
		opts = modelgroup.Options{PreferLastOK: true, SniffEnabled: true}.WithDefaults()
	}

	out := make([]map[string]any, 0, len(all))
	for _, g := range all {
		members := make([]map[string]any, 0, len(g.Members))
		for _, m := range g.Members {
			item := map[string]any{
				"model":     m.Model,
				"realm":     m.Realm,
				"bare":      m.Bare,
				"weight":    m.Weight,
				"enabled":   m.Enabled,
				"in_window": m.IsVisible(now),
			}
			// window 输出规范化的 "HH:MM-HH:MM"（不带 String() 的「(跨天)」后缀）：
			// 前端要把它回填到 <input type="time">，后缀会让保存时解析失败。
			// 「跨天」由前端按 start > end 自行判定并加标签。
			item["window"] = windowText(m.Window)
			// 锁状态：与「模型锁池」表逐字一致
			if row, ok := lockIdx[m.Realm+"\x1f"+m.Bare]; ok {
				item["state"] = row.State
				item["servable"] = row.Servable
				item["total"] = row.Total
				item["locked"] = row.Locked
				item["reason"] = row.Reason
				if !row.UnlockAt.IsZero() {
					item["unlock_at"] = row.UnlockAt.Format(time.RFC3339)
				}
			} else {
				// 无锁行 = 该模型此刻未被锁。此时 total/servable 必须现算，不能写死 0：
				//   - 域内一个可路由账号都没有（全被 disabled/paused）→ 0/0
				//   - 域内有账号但该模型未被锁 → N/N（全部可服务）
				// 旧实现把两者都填 0，前端又把 0 渲染成「全部」，导致「没有账号」
				// 被显示成「全部账号可服务」—— 用户把国际账号全暂停后，
				// global 成员仍显示「全部/全部」，与实际（0/0）相反。
				item["state"] = ""
				item["servable"] = 0
				item["total"] = 0
				item["locked"] = 0
				if p.cfg.Pool != nil {
					// total 用「可路由账号数」而非全部账号数：被停用/暂停选号的账号
					// 不参与任何选号，对运维而言等同于「这个域没有账号」。
					// 这正是用户报的场景：把国际账号全部暂停后，global 成员
					// 应该显示 0/0，而不是「全部/全部」。
					h := p.cfg.Pool.RealmHealth(m.Realm)
					item["total"] = h.Total - h.Disabled - h.Paused
					// servable 用 healthy：已排除停用/暂停/账号级冷却，与选号口径一致。
					item["servable"] = h.Healthy
				}
			}
			// 探活时刻与锁因分类是 Cache 独有字段（展示状态仍以 ModelLockView 为准）：
			//   probed_at —— 「上次探活」列
			//   kind      —— 决定「能不能嗅探」：只有 model_rate 值得真探活，
			//                model_missing / hard_credit 探了也必然失败，等 TTL 即可。
			//                前端据此把「嗅探」按钮置灰为「等 TTL」，不必自己猜。
			item["kind"] = ""
			item["can_sniff"] = false
			if cache != nil {
				if st, ok := cache.Get(m.Realm, m.Bare); ok {
					if !st.ProbedAt.IsZero() {
						item["probed_at"] = st.ProbedAt.Format(time.RFC3339)
					}
					item["kind"] = string(st.Kind)
					// 可嗅探 = 缓存说不可用 + 锁因是 model_rate。
					// 已经可用的成员没必要探（RunOnce 也会跳过）。
					item["can_sniff"] = !st.Usable && st.Kind == modelgroup.KindModelRate
				}
			}
			members = append(members, item)
		}
		out = append(out, map[string]any{
			"id":      g.ID,
			"name":    g.Name,
			"enabled": g.Enabled,
			"members": members,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"groups": out,
		"options": map[string]any{
			"prefer_last_ok":  opts.PreferLastOK,
			"sniff_enabled":   opts.SniffEnabled,
			"sniff_interval":  opts.SniffInterval.String(),
			"sniff_min_until": opts.SniffMinUntil.String(),
			"sniff_min_gap":   opts.SniffMinGap.String(),
		},
	})
}

// modelGroupsSniff 手动触发一轮嗅探（面板「立即嗅探全部」）。
//
// 为什么不在面板里重写一遍「哪些成员该探」：那套判据（可见性过滤、锁因过滤、
// 剩余冷却下限、探活频率上限）已经在 Sniffer.RunOnce 里，重复实现必然漂移。
// 这里只是把它调一次，然后回报结果。
//
// 语义是「尽力而为的一轮」，不是「强制探活」：
// 刚探过的成员会被 sniff_min_gap 跳过，返回的 probed 可能小于用户预期 ——
// 这是设计上的频率保护，前端要把这个事实说清楚，而不是显示成「失败了」。
func (p *Panel) modelGroupsSniff(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SniffModelGroups == nil {
		writeErr(w, http.StatusNotImplemented, "sniffing model groups is not wired")
		return
	}
	// 一轮嗅探要逐账号发真实请求，可能持续数十秒；给它独立于面板其余接口的超时。
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	probed, recovered, err := p.cfg.SniffModelGroups(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "sniff: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"probed":    probed,
		"recovered": recovered,
	})
}

// modelGroupsPost 保存组配置。校验失败返回 400 且不落盘。
func (p *Panel) modelGroupsPost(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveModelGroups == nil {
		writeErr(w, http.StatusNotImplemented, "saving model groups is not wired")
		return
	}
	var req struct {
		Groups  []modelgroup.GroupConfig `json:"groups"`
		Options struct {
			PreferLastOK  bool   `json:"prefer_last_ok"`
			SniffEnabled  bool   `json:"sniff_enabled"`
			SniffInterval string `json:"sniff_interval"`
			SniffMinUntil string `json:"sniff_min_until"`
			SniffMinGap   string `json:"sniff_min_gap"`
		} `json:"options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}

	// 校验：这些是「保存后点名必失败」的配置错误，必须在落盘前拦住。
	seen := make(map[string]struct{}, len(req.Groups))
	for _, g := range req.Groups {
		id := strings.ToLower(strings.TrimSpace(g.ID))
		if id == "" {
			writeErr(w, http.StatusBadRequest, "group id must not be empty")
			return
		}
		if _, dup := seen[id]; dup {
			writeErr(w, http.StatusBadRequest, "duplicate group id: "+id)
			return
		}
		seen[id] = struct{}{}
		if len(g.Members) == 0 {
			writeErr(w, http.StatusBadRequest, "group "+id+" has no member")
			return
		}
		for _, m := range g.Members {
			if strings.TrimSpace(m.Model) == "" {
				writeErr(w, http.StatusBadRequest, "group "+id+" has a member with empty model")
				return
			}
		}
	}

	opts := modelgroup.Options{
		PreferLastOK: req.Options.PreferLastOK,
		SniffEnabled: req.Options.SniffEnabled,
	}
	for _, f := range []struct {
		name string
		raw  string
		dst  *time.Duration
	}{
		{"sniff_interval", req.Options.SniffInterval, &opts.SniffInterval},
		{"sniff_min_until", req.Options.SniffMinUntil, &opts.SniffMinUntil},
		{"sniff_min_gap", req.Options.SniffMinGap, &opts.SniffMinGap},
	} {
		if f.raw == "" {
			continue
		}
		d, err := time.ParseDuration(f.raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, f.name+": "+err.Error())
			return
		}
		if d <= 0 {
			writeErr(w, http.StatusBadRequest, f.name+" must be positive")
			return
		}
		*f.dst = d
	}

	if err := p.cfg.SaveModelGroups(req.Groups, opts); err != nil {
		writeErr(w, http.StatusInternalServerError, "save: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// windowText 把 *Window 渲染成可回填的 "HH:MM-HH:MM"（nil = 空串 = 全天）。
func windowText(w *modelgroup.Window) string {
	if w == nil {
		return ""
	}
	f := func(v int) string { return fmt.Sprintf("%02d:%02d", v/60, v%60) }
	return f(w.Start) + "-" + f(w.End)
}

// groupSet 当前组集合（nil 安全）。
func (p *Panel) groupSet() *modelgroup.GroupSet {
	if p.cfg.GroupSet == nil {
		return nil
	}
	return p.cfg.GroupSet()
}

// groupCache 当前缓存（nil 安全）。
func (p *Panel) groupCache() *modelgroup.Cache {
	if p.cfg.GroupCache == nil {
		return nil
	}
	return p.cfg.GroupCache()
}
