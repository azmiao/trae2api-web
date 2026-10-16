// 管理面板（/admin）：只读查询，无鉴权（本地面板）；CLI 操作留待开发。
package server

import (
	_ "embed"
	"net/http"
	"sync"
	"time"

	"trae2api-web/internal/pool"
)

//go:embed admin.html
var adminPageHTML []byte

// adminPage 返回内嵌 HTML 面板（深色简洁风，无外部依赖）。
func (h *Handler) adminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(adminPageHTML)
}

// adminCredits 查询全部账号的实时额度 + 签到状态（并发拉取上游）。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	type acct struct {
		UID            string `json:"uid"`
		Nickname       string `json:"nickname"`
		Remain         int64  `json:"remain"`
		Limit          int64  `json:"limit"`
		Used           int64  `json:"used"`
		Packs          int    `json:"packs"`
		CheckedIn      bool   `json:"checked_in"`
		CheckinCredits int64  `json:"checkin_credits"`
		CheckinEnable  bool   `json:"checkin_enable"`
		Cooling        bool   `json:"cooling"`
		Disabled       bool   `json:"disabled"`
		Error          string `json:"error,omitempty"`
	}

	st := h.cfg.Pool.List()
	out := make([]acct, len(st))
	var wg sync.WaitGroup
	for i, s := range st {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			a := h.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				out[i] = acct{UID: s.UID, Nickname: s.Nickname, Error: "no auth found"}
				return
			}
			var ac acct
			ac.UID = s.UID
			ac.Nickname = s.Nickname
			ac.Cooling = s.Cooling
			ac.Disabled = s.Disabled
			// 优先使用缓存的 SOLO ide_credits；为 0 时回退 entitlement 聚合（可能含 work_credits）
			_, limit, used, packs, err := h.cfg.Upstream.EntUsage(a)
			if err != nil {
				ac.Error = "ent_usage: " + err.Error()
			} else {
				ac.Limit, ac.Used, ac.Packs = limit, used, packs
				// 首次加载时把 entitlement 聚合值写入缓存，避免后续显示为空；
				// 已有缓存（来自聊天响应的真实 ide_credits）则优先保留。
				if s.Credits <= 0 {
					remain, _ := h.cfg.Upstream.UserEntUsage(a)
					ac.Remain = remain
				} else {
					ac.Remain = s.Credits
				}
			}
			checkedIn, credits, enable, cerr := h.cfg.Upstream.CheckinStatus(a)
			if cerr != nil {
				if ac.Error != "" {
					ac.Error += "; "
				}
				ac.Error += "checkin: " + cerr.Error()
			} else {
				ac.CheckedIn, ac.CheckinCredits, ac.CheckinEnable = checkedIn, credits, enable
			}
			out[i] = ac
		}(i, s)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{
		"fetched_at": time.Now().Format("2006-01-02 15:04:05"),
		"accounts":   out,
	})
}

// adminCreditsRefreshAll POST /admin/api/credits/refresh：后台批量刷新所有账号的 entitlement 额度。
// 已有 ide_credits 缓存的账号跳过，避免被 work 聚合值覆盖。
func (h *Handler) adminCreditsRefreshAll(w http.ResponseWriter, r *http.Request) {
	st := h.cfg.Pool.List()
	refreshed, skipped := 0, 0
	for _, s := range st {
		if s.Credits > 0 {
			skipped++
			continue
		}
		a := h.cfg.Pool.AuthByUID(s.UID)
		if a == nil {
			continue
		}
		remain, err := h.cfg.Upstream.UserEntUsage(a)
		if err != nil {
			continue
		}
		h.cfg.Pool.SetCredits(s.UID, remain)
		refreshed++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"refreshed": refreshed,
		"skipped":   skipped,
	})
}
