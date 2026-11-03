package config

import ierr "opscollector/internal/errors"

func wantCode(err error, code string) bool {
	return err != nil && ierr.CodeOf(err) == code
}
