package refract

// Renameat is a wrapper around renameat syscall.
// On Linux, it is a wrapper around renameat2(2).
// On Darwin, it is a wrapper around renameatx_np(2).
func Renameat(olddirfd int, oldpath string, newdirfd int, newpath string, flags uint) (err error) {
	return renameat(olddirfd, oldpath, newdirfd, newpath, flags)
}
