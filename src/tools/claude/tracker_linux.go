package claude

import (
	"os"
	"syscall"
	"time"
)

func changeTime(info os.FileInfo) time.Time {
	s := info.Sys().(*syscall.Stat_t)
	return time.Unix(s.Ctim.Sec, s.Ctim.Nsec)
}
