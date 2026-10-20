package install

import "syscall"

func statTimes(st *syscall.Stat_t) (mtimeMs, ctimeMs float64) {
	return float64(st.Mtim.Sec)*1e3 + float64(st.Mtim.Nsec)/1e6,
		float64(st.Ctim.Sec)*1e3 + float64(st.Ctim.Nsec)/1e6
}
