package main

import (
	"encoding/base64"
	"errors"

	"github.com/Shpuntyara/wms-platform/internal/inventory"
	"github.com/jackc/pgx/v5"
)

type databaseProfile struct {
	hardened   bool
	contextKey []byte
	workerURL  string
}

func parseDatabaseProfile(name, encodedKey, workerURL string) (databaseProfile, error) {
	switch name {
	case "legacy":
		if encodedKey != "" || workerURL != "" {
			return databaseProfile{}, errors.New("request context and worker credentials require WMS_DATABASE_PROFILE=onprem-v1")
		}
		return databaseProfile{}, nil
	case "", "onprem-v1":
		key, err := base64.StdEncoding.DecodeString(encodedKey)
		if err != nil || len(key) != 32 || workerURL == "" {
			return databaseProfile{}, errors.New("onprem-v1 requires a separate worker database URL and a base64 32-byte request-context key")
		}
		return databaseProfile{hardened: true, contextKey: key, workerURL: workerURL}, nil
	case "hardened-v1":
		return databaseProfile{}, errors.New("the single-warehouse profile requires the onprem-v1 database upgrade and configuration")
	default:
		return databaseProfile{}, errors.New("unknown WMS_DATABASE_PROFILE")
	}
}

func validateHardenedWorkerTarget(api, worker *pgx.ConnConfig) error {
	if api == nil || worker == nil || api.User == worker.User || api.Host != worker.Host || api.Port != worker.Port || api.Database != worker.Database {
		return errors.New("hardened API and worker must use distinct logins on the same database endpoint")
	}
	for _, config := range []*pgx.ConnConfig{api, worker} {
		for _, fallback := range config.Fallbacks {
			if fallback.Host != config.Host || fallback.Port != config.Port {
				return errors.New("hardened database fallbacks must remain on the configured deployment endpoint")
			}
		}
	}
	return nil
}

func validateOnPremBinding(apiLogin, workerLogin, apiInstallation, workerInstallation string) error {
	apiID, apiErr := inventory.NormalizeID(apiInstallation)
	workerID, workerErr := inventory.NormalizeID(workerInstallation)
	if apiLogin == "" || workerLogin == "" || apiLogin == workerLogin || apiErr != nil || workerErr != nil || apiID != workerID {
		return errors.New("on-premise API and worker require distinct effective logins on the same client installation")
	}
	return nil
}
