//go:build !windows

package service

import "qindexer/internal/config"

func stopPrivatePostgres(string, *config.Config) error {
	return nil
}
