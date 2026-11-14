// prober.go 成员级探活的实现：按该域账号逐个试，探到第一个成功即停。
//
// 单个账号的探活结果不能直接写成成员级结论 —— 成员恢复的判据是
// 「该域至少还有一个账号能服务这个模型」。
package server

import (
	"context"
	"errors"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/modelgroup"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// probePool 探活只需要这几个能力（便于测试替换）。
type probePool interface {
	AvailableUIDsForRealm(realm string) []string
	AuthByUID(uid string) *auth.Auth
	Acquire(uid string) bool
	Release(uid string)
}

// memberProber 基于 upstream.Client 的成员级探活。
type memberProber struct {
	pool     probePool
	upstream *upstream.Client
}

// NewMemberProber 构造探活器（main 装配用）。
func NewMemberProber(p probePool, up *upstream.Client) modelgroup.Prober {
	return &memberProber{pool: p, upstream: up}
}

// Probe 逐账号发一次最小请求（max_tokens=1，非流式）。
//
// 返回 ok=true 表示至少一个账号能服务该模型（成员级恢复）。
// ok=false 时 until/reason 取该域所有账号里最早的解封线索。
func (p *memberProber) Probe(ctx context.Context, realm, bare string) (bool, time.Time, string) {
	if p == nil || p.pool == nil || p.upstream == nil {
		return false, time.Time{}, ""
	}
	uids := p.pool.AvailableUIDsForRealm(realm)
	var earliest time.Time
	var reason string
	for _, uid := range uids {
		if ctx.Err() != nil {
			break
		}
		acct := p.pool.AuthByUID(uid)
		if acct == nil {
			continue
		}
		// 占在途名额：探活也是真实请求，不能与用户请求抢并发。
		if !p.pool.Acquire(uid) {
			continue
		}
		body := []byte(`{"model":"` + bare + `","messages":[{"role":"user","content":"ping"}],"max_tokens":1,"stream":false}`)
		rc, status, respBody, err := p.upstream.ChatStreamContext(ctx, acct, body, "", upstream.ChatMeta{}, nil)
		if rc != nil {
			rc.Close()
		}
		p.pool.Release(uid)
		if err == nil && status < 400 {
			return true, time.Time{}, ""
		}
		// 该账号仍失败：收集最早解封线索，换下一个账号
		var uerr *upstream.Error
		if errors.As(err, &uerr) && uerr.RetryAfter > 0 {
			at := time.Now().Add(uerr.RetryAfter)
			if earliest.IsZero() || at.Before(earliest) {
				earliest = at
			}
		}
		if resetAt, ok := upstream.ParseRateReset(string(respBody)); ok {
			if earliest.IsZero() || resetAt.Before(earliest) {
				earliest = resetAt
			}
		}
		if reason == "" && len(respBody) > 0 {
			reason = string(respBody)
		}
	}
	return false, earliest, reason
}

var _ modelgroup.Prober = (*memberProber)(nil)
