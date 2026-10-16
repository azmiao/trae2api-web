package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

func TestAdminCheckinAccountClaimsAndRefreshesCredits(t *testing.T) {
	var statusCalls, claimCalls, usageCalls atomic.Int32
	up := &upstream.Client{
		HTTP: checkinRoundTripClient(func(r *http.Request) (string, error) {
			switch r.URL.Path {
			case upstream.EpCheckinStatus:
				statusCalls.Add(1)
				return `{"checked_in":false,"credits":200,"enable":true}`, nil
			case upstream.EpCheckinClaim:
				claimCalls.Add(1)
				return `{"code":0,"message":"success"}`, nil
			case upstream.EpEntUsage:
				usageCalls.Add(1)
				return `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":0}}]}`, nil
			default:
				return "", &testCheckinError{path: r.URL.Path}
			}
		}),
		UgHost: "https://fake.example",
	}

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	p.Cooldown("u1", pool.CoolPlan, time.Hour, "plan limit")
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "test-key"})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/u1/checkin", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not json: %v", err)
	}
	if got["claimed"] != true || got["checked_in"] != true {
		t.Fatalf("unexpected checkin result: %s", rec.Body)
	}
	if got["credits"] != float64(500) {
		t.Errorf("credits=%v want 500", got["credits"])
	}
	if statusCalls.Load() != 1 || claimCalls.Load() != 1 || usageCalls.Load() != 1 {
		t.Errorf("calls status=%d claim=%d usage=%d", statusCalls.Load(), claimCalls.Load(), usageCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Credits != 500 {
		t.Errorf("pool status=%+v", st)
	}
}

func TestAdminCheckinAccountDoesNotClaimTwice(t *testing.T) {
	var claimCalls atomic.Int32
	up := &upstream.Client{
		HTTP: checkinRoundTripClient(func(r *http.Request) (string, error) {
			switch r.URL.Path {
			case upstream.EpCheckinStatus:
				return `{"checked_in":true,"credits":0,"enable":false}`, nil
			case upstream.EpCheckinClaim:
				claimCalls.Add(1)
				return `{"code":0}`, nil
			case upstream.EpEntUsage:
				return `{"user_entitlement_pack_list":[]}`, nil
			default:
				return "", &testCheckinError{path: r.URL.Path}
			}
		}),
		UgHost: "https://fake.example",
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/u1/checkin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"checked_in":true`) {
		t.Errorf("body=%s", rec.Body)
	}
	if claimCalls.Load() != 0 {
		t.Errorf("already checked in account should not claim, calls=%d", claimCalls.Load())
	}
}

type testCheckinError struct{ path string }

func (e *testCheckinError) Error() string { return "unexpected path: " + e.path }

type checkinRoundTripFunc func(*http.Request) (string, error)

func (f checkinRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
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

func checkinRoundTripClient(f checkinRoundTripFunc) *http.Client {
	return &http.Client{Transport: f}
}
