package core

import (
	"os"
	"syscall"
	"time"
)

func changeTime(info os.FileInfo) time.Time {
	s := info.Sys().(*syscall.Stat_t)
	return time.Unix(s.Ctimespec.Sec, s.Ctimespec.Nsec)
}
