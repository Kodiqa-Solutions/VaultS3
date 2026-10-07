package api

import (
	"net/http"
	"strconv"
)

// Public list prices (mid-2026), $/GB. Estimates only — actual pricing varies by
// region, storage class, and committed volume. Egress is where managed S3 gets
// expensive; self-hosted VaultS3 is egress-free.
var tcoPricing = []struct {
	name    string
	storage float64 // $/GB/month
	egress  float64 // $/GB
}{
	{"AWS S3 Standard", 0.023, 0.09},
	{"Google Cloud Storage", 0.020, 0.12},
	{"Cloudflare R2", 0.015, 0.0},
	{"Backblaze B2", 0.006, 0.01},
	{"Wasabi", 0.0069, 0.0},
}

const gib = 1024 * 1024 * 1024

type tcoProvider struct {
	Name             string  `json:"name"`
	StorageRatePerGb float64 `json:"storageRatePerGb"`
	EgressRatePerGb  float64 `json:"egressRatePerGb"`
	StorageCost      float64 `json:"storageCost"`
	EgressCost       float64 `json:"egressCost"`
	MonthlyTotal     float64 `json:"monthlyTotal"`
}

type tcoResponse struct {
	StorageBytes int64         `json:"storageBytes"`
	StorageGb    float64       `json:"storageGb"`
	EgressGb     float64       `json:"egressGb"`
	Providers    []tcoProvider `json:"providers"`
	Note         string        `json:"note"`
}

// computeTCO estimates the monthly managed-cloud cost of the given stored bytes
// and assumed monthly egress. Self-hosted VaultS3 is $0 (egress-free).
func computeTCO(storageBytes int64, egressGb float64) tcoResponse {
	storageGb := float64(storageBytes) / gib
	resp := tcoResponse{
		StorageBytes: storageBytes,
		StorageGb:    storageGb,
		EgressGb:     egressGb,
		Note:         "Estimated monthly cost at public list prices (mid-2026); actual pricing varies by region, storage class, and committed volume. Self-hosted VaultS3 is egress-free ($0).",
	}
	for _, p := range tcoPricing {
		sc := storageGb * p.storage
		ec := egressGb * p.egress
		resp.Providers = append(resp.Providers, tcoProvider{
			Name:             p.name,
			StorageRatePerGb: p.storage,
			EgressRatePerGb:  p.egress,
			StorageCost:      sc,
			EgressCost:       ec,
			MonthlyTotal:     sc + ec,
		})
	}
	return resp
}

// handleTCO handles GET /api/v1/tco?egress_gb=N — estimates what this data would
// cost on managed S3-compatible clouds vs. self-hosting (free).
func (h *APIHandler) handleTCO(w http.ResponseWriter, r *http.Request) {
	var storageBytes int64
	buckets, _ := h.store.ListBuckets()
	// A non-admin is costed on the buckets they may list, not on everyone's.
	visible := h.visibleBucketSet(r, buckets)
	for _, b := range buckets {
		if visible != nil && !visible[b.Name] {
			continue
		}
		size, _ := h.bucketStatCounter(b.Name)
		storageBytes += size
	}

	egressGb := -1.0
	if v := r.URL.Query().Get("egress_gb"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			egressGb = f
		}
	}
	if egressGb < 0 {
		// Default assumption: you serve your whole dataset about once a month.
		egressGb = float64(storageBytes) / gib
	}

	writeJSON(w, http.StatusOK, computeTCO(storageBytes, egressGb))
}
