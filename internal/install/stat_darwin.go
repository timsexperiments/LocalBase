package install

import "syscall"

func statTimes(st *syscall.Stat_t) (mtimeMs, ctimeMs float64) {
	return float64(st.Mtimespec.Sec)*1e3 + float64(st.Mtimespec.Nsec)/1e6,
		float64(st.Ctimespec.Sec)*1e3 + float64(st.Ctimespec.Nsec)/1e6
}
