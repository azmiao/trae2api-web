package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

func TestAdminRefreshCreditsUsesCachedIDECredits(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.SetCredits("u1", 1234) // 已缓存真实 SOLO ide_credits

	h := NewHandler(Config{Pool: p})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/u1/credits/refresh", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["source"] != "cached_ide_credits" {
		t.Errorf("source=%v want cached_ide_credits", got["source"])
	}
	if got["credits"] != float64(1234) {
		t.Errorf("credits=%v want 1234", got["credits"])
	}
}

func TestAdminRefreshCreditsFallbackToEntitlement(t *testing.T) {
	var calls atomic.Int32
	up := &upstream.Client{
		HTTP: creditsRoundTripClient(func(r *http.Request) (string, error) {
			calls.Add(1)
			return `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":2000}},"usage":{"credits_amount":0}}]}`, nil
		}),
		UgHost: "https://fake.example",
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	// 不预设 credits → 应回退 entitlement 查询

	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/u1/credits/refresh", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["source"] != "entitlement_fallback" {
		t.Errorf("source=%v want entitlement_fallback", got["source"])
	}
	if got["credits"] != float64(2000) {
		t.Errorf("credits=%v want 2000", got["credits"])
	}
	if calls.Load() == 0 {
		t.Error("upstream should be called when no cached credits")
	}
}

func TestAdminRefreshCreditsMissingAccount(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/missing/credits/refresh", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestChatNonStreamUpdatesPoolCreditsFromNotifyUsage(t *testing.T) {
	sseWithNotify := "event:metadata\ndata:{\"model\":\"\",\"session_id\":\"s1\"}\n\n" +
		"event:output\ndata:{\"response\":\"ok\",\"reasoning_content\":\"\",\"tool_calls\":null}\n\n" +
		"event:notify_usage\ndata:{\"cn_credits_remain_info\":{\"ide_credits\":777,\"work_credits\":2000}}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseWithNotify, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Credits != 777 {
		t.Errorf("pool credits=%d want 777 (from notify_usage.ide_credits)", st.Credits)
	}
}

func TestChatStreamUpdatesPoolCreditsFromNotifyUsage(t *testing.T) {
	sseWithNotify := "event:output\ndata:{\"response\":\"hi\",\"reasoning_content\":\"\",\"tool_calls\":null}\n\n" +
		"event:notify_usage\ndata:{\"cn_credits_remain_info\":{\"ide_credits\":555,\"work_credits\":2000}}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseWithNotify, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.Credits != 555 {
		t.Errorf("pool credits=%d want 555", st.Credits)
	}
}

type creditsRoundTripFunc func(*http.Request) (string, error)

func (f creditsRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := f(r)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func creditsRoundTripClient(f creditsRoundTripFunc) *http.Client {
	return &http.Client{Transport: f}
}
