//go:build !linux

package fio

import "errors"

func probeIoUringSupport() error {
	return errors.New("io-uring is only available on linux")
}
