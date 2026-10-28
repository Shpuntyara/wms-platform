package main

import (
	"encoding/base64"
	"errors"

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
			return databaseProfile{}, errors.New("request context and worker credentials require WMS_DATABASE_PROFILE=hardened-v1")
		}
		return databaseProfile{}, nil
	case "", "hardened-v1":
		key, err := base64.StdEncoding.DecodeString(encodedKey)
		if err != nil || len(key) != 32 || workerURL == "" {
			return databaseProfile{}, errors.New("hardened-v1 requires a separate worker database URL and a base64 32-byte request-context key")
		}
		return databaseProfile{hardened: true, contextKey: key, workerURL: workerURL}, nil
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

func validateHardenedBinding(apiLogin, workerLogin, apiOrg, apiWarehouse, workerOrg, workerWarehouse string) error {
	if apiLogin == "" || workerLogin == "" || apiLogin == workerLogin || apiOrg == "" || apiWarehouse == "" || apiOrg != workerOrg || apiWarehouse != workerWarehouse {
		return errors.New("hardened API and worker require distinct effective logins bound to the same organization and warehouse")
	}
	return nil
}
