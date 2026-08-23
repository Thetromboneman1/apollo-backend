package api

import (
	"encoding/json"
	"net/http"

	"go.uber.org/zap"
)

// reqV2Handler is a diagnostic stub for `POST /api/req_v2`. Apollo iOS
// posts to this endpoint repeatedly (originally against apolloreq.com,
// rewritten to this backend by the tweak). The endpoint shape is not
// public, so the bounded body is discarded and a permissive empty response is
// returned. Request contents are never logged.
func (a *api) reqV2Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

// checkReceiptHandler stubs `POST /v1/receipt/{apns}`. Apollo iOS posts
// the base64-encoded App Store receipt here and uses the response to
// decide whether the user has Apollo Pro / Ultra. On a sideloaded build
// the receipt is synthetic (or empty) and there's no Apple to validate
// it against; this handler unconditionally claims Pro + Ultra are
// active so notification setup proceeds.
//
// The response shape is the historical `ClientVerificationInfo` from
// the upstream apollo-backend's `internal/itunes/receipt.go` (commit
// 6e4b485^). Product names mirror the strings Apollo's binary checks
// against (apollo_pro_*, apollo_ultra).
func (a *api) checkReceiptHandler(w http.ResponseWriter, r *http.Request) {
	type product struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		SubscriptionType string `json:"subscription_type,omitempty"`
	}
	type clientVerificationInfo struct {
		Products []product `json:"products"`
	}

	// Status / name / subscription_type vocabulary from the original
	// upstream backend (commit 6e4b485^ in internal/itunes/receipt.go).
	// "LIFETIME" is the catch-all for "user owns this one-time-purchase
	// product"; for the Ultra subscription it doubles as "lifetime sub".
	resp := clientVerificationInfo{
		Products: []product{
			{Name: "ultra", Status: "LIFETIME", SubscriptionType: "LIFETIME"},
			{Name: "pro", Status: "LIFETIME"},
			{Name: "community_icons", Status: "LIFETIME"},
			{Name: "spca", Status: "LIFETIME"},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// announcementHandler stubs `GET /api/announcement` — Apollo polls this
// at app launch and shows a banner if there's content. Returning an
// empty 200 silences the 404 noise without changing behavior.
func (a *api) announcementHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

// notFoundLogger intentionally records neither request body nor raw path. An
// unmatched request can contain credentials or device tokens, and it does not
// have a safe route template to log.
func (a *api) notFoundLogger(w http.ResponseWriter, r *http.Request) {
	a.logger.Info("unmatched route",
		zap.String("method", r.Method),
		zap.String("route", "unmatched"),
	)
	http.NotFound(w, r)
}
