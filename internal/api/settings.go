package api

import (
	"crypto/hmac"
	"log/slog"
	"net/http"
)

type settingsResponse struct {
	Server struct {
		Address             string `json:"address"`
		Port                int    `json:"port"`
		Domain              string `json:"domain,omitempty"`
		ShutdownTimeoutSecs int    `json:"shutdownTimeoutSecs"`
		TLSEnabled          bool   `json:"tlsEnabled"`
	} `json:"server"`
	Storage struct {
		DataDir               string `json:"dataDir"`
		MetadataDir           string `json:"metadataDir"`
		UsageScanIntervalSecs int    `json:"usageScanIntervalSecs"`
	} `json:"storage"`
	Features struct {
		Encryption          bool `json:"encryption"`
		Compression         bool `json:"compression"`
		AccessLog           bool `json:"accessLog"`
		RateLimit           bool `json:"rateLimit"`
		Replication         bool `json:"replication"`
		Scanner             bool `json:"scanner"`
		Tiering             bool `json:"tiering"`
		Backup              bool `json:"backup"`
		OIDC                bool `json:"oidc"`
		Lambda              bool `json:"lambda"`
		Vector              bool `json:"vector"`
		Erasure             bool `json:"erasure"`
		Cluster             bool `json:"cluster"`
		Packing             bool `json:"packing"`
		PerBucketEncryption bool `json:"perBucketEncryption"`
		ExternalAuth        bool `json:"externalAuth"`
		Debug               bool `json:"debug"`
	} `json:"features"`
	Lifecycle struct {
		ScanIntervalSecs   int `json:"scanIntervalSecs"`
		AuditRetentionDays int `json:"auditRetentionDays"`
	} `json:"lifecycle"`
	RateLimit struct {
		RequestsPerSec float64 `json:"requestsPerSec"`
		BurstSize      int     `json:"burstSize"`
		PerKeyRPS      float64 `json:"perKeyRps"`
		PerKeyBurst    int     `json:"perKeyBurst"`
	} `json:"rateLimit,omitempty"`
	Memory struct {
		MaxSearchEntries int `json:"maxSearchEntries"`
		GoMemLimitMB     int `json:"goMemLimitMb,omitempty"`
	} `json:"memory"`
}

func (h *APIHandler) handleSettings(w http.ResponseWriter, _ *http.Request) {
	var resp settingsResponse

	resp.Server.Address = h.cfg.Server.Address
	resp.Server.Port = h.cfg.Server.Port
	resp.Server.Domain = h.cfg.Server.Domain
	resp.Server.ShutdownTimeoutSecs = h.cfg.Server.ShutdownTimeoutSecs
	resp.Server.TLSEnabled = h.cfg.Server.TLS.Enabled

	resp.Storage.DataDir = h.cfg.Storage.DataDir
	resp.Storage.MetadataDir = h.cfg.Storage.MetadataDir
	resp.Storage.UsageScanIntervalSecs = h.cfg.Storage.UsageScanIntervalSecs

	resp.Features.Encryption = h.cfg.Encryption.Enabled
	resp.Features.PerBucketEncryption = h.cfg.Encryption.PerBucket
	resp.Features.Compression = h.cfg.Compression.Enabled
	resp.Features.Packing = h.cfg.Packing.Enabled
	resp.Features.AccessLog = h.cfg.Logging.Enabled
	resp.Features.RateLimit = h.cfg.RateLimit.Enabled
	resp.Features.Replication = h.cfg.Replication.Enabled
	resp.Features.Scanner = h.cfg.Scanner.Enabled
	resp.Features.Tiering = h.cfg.Tiering.Enabled
	resp.Features.Backup = h.cfg.Backup.Enabled
	resp.Features.OIDC = h.cfg.OIDC.Enabled
	resp.Features.Lambda = h.cfg.Lambda.Enabled
	resp.Features.Vector = h.cfg.Vector.Enabled
	resp.Features.Erasure = h.cfg.Erasure.Enabled
	resp.Features.Cluster = h.cfg.Cluster.Enabled
	resp.Features.ExternalAuth = h.cfg.ExternalAuth.Enabled
	resp.Features.Debug = h.cfg.Debug

	resp.Lifecycle.ScanIntervalSecs = h.cfg.Lifecycle.ScanIntervalSecs
	resp.Lifecycle.AuditRetentionDays = h.cfg.Security.AuditRetentionDays

	if h.cfg.RateLimit.Enabled {
		resp.RateLimit.RequestsPerSec = h.cfg.RateLimit.RequestsPerSec
		resp.RateLimit.BurstSize = h.cfg.RateLimit.BurstSize
		resp.RateLimit.PerKeyRPS = h.cfg.RateLimit.PerKeyRPS
		resp.RateLimit.PerKeyBurst = h.cfg.RateLimit.PerKeyBurst
	}

	resp.Memory.MaxSearchEntries = h.cfg.Memory.MaxSearchEntries
	resp.Memory.GoMemLimitMB = h.cfg.Memory.GoMemLimitMB

	writeJSON(w, http.StatusOK, resp)
}

type changeCredentialsRequest struct {
	CurrentSecretKey string `json:"currentSecretKey"`
	NewAccessKey     string `json:"newAccessKey"`
	NewSecretKey     string `json:"newSecretKey"`
}

func (h *APIHandler) handleChangeCredentials(w http.ResponseWriter, r *http.Request) {
	var req changeCredentialsRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.NewAccessKey == "" || req.NewSecretKey == "" {
		writeError(w, http.StatusBadRequest, "new access key and secret key are required")
		return
	}

	if len(req.NewSecretKey) < 8 {
		writeError(w, http.StatusBadRequest, "secret key must be at least 8 characters")
		return
	}

	// Verify current secret key
	if !hmac.Equal([]byte(req.CurrentSecretKey), []byte(h.cfg.Auth.AdminSecretKey)) {
		writeError(w, http.StatusForbidden, "current secret key is incorrect")
		return
	}

	// Update in-memory config
	h.cfg.Auth.AdminAccessKey = req.NewAccessKey
	h.cfg.Auth.AdminSecretKey = req.NewSecretKey

	// Update S3 authenticator so SigV4 auth uses new credentials
	if h.s3Auth != nil {
		h.s3Auth.UpdateAdminCredentials(req.NewAccessKey, req.NewSecretKey)
	}

	// Rotate the console signing key so sessions issued under the old password
	// stop working, which is what changing a password is supposed to do. The new
	// key is random rather than derived from the new secret: a key derived from a
	// credential is forgeable by anyone who learns that credential.
	if key, err := NewRandomJWTKey(); err == nil {
		h.jwt = NewJWTService(key)
		if h.jwtKeyStore != nil {
			if err := h.jwtKeyStore.SetJWTSigningKey(key); err != nil {
				slog.Error("could not persist the rotated console signing key, sessions will not survive a restart", "error", err)
			}
		}
	} else {
		slog.Error("could not rotate the console signing key", "error", err)
	}

	// Persist to metadata store
	if h.store != nil {
		_ = h.store.SetAdminCredentials(req.NewAccessKey, req.NewSecretKey)
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "credentials updated successfully"})
}
