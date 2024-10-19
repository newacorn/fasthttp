package fasthttp

func AppendUintInto(dst []byte, n int) []byte {
	if n < 0 {
		// developer sanity-check
		panic("BUG: int must be positive")
	}
	i := len(dst)
	var q int
	for n >= 10 {
		i--
		q = n / 10
		dst[i] = '0' + byte(n-q*10)
		n = q
	}
	i--
	dst[i] = '0' + byte(n)
	return dst[i:]
}
