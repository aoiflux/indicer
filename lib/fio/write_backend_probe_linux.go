//go:build linux

package fio

func probeIoUringSupport() error {
	h, err := openIoUringHandle()
	if err != nil {
		return err
	}
	return h.Close()
}
