package fasthttp

import (
	"testing"
)

func BenchmarkSetUint(b *testing.B) {
	arg := AcquireArgs()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		arg.SetUint("b", 121232323)
	}
	ReleaseArgs(arg)
}

func BenchmarkSetUintWithoutPool(b *testing.B) {
	arg := AcquireArgs()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		arg.SetUintInto("b", 121232323)
	}
	ReleaseArgs(arg)
}

// SetUint sets uint value for the given key.
func (a *Args) SetUintInto(key string, value int) {
	//bb := bytebufferpool.Get()
	dst := make([]byte, 20)
	dst = AppendUintInto(dst, value)
	a.SetBytesV(key, dst)
}

/*

func TestDecode(t *testing.T) {
	dataLen := [10]int{0, 10, 200, 65536}
	for _, d := range dataLen {
		dataStr := randomstring.HumanFriendlyString(d)
		dataBytes := []byte(dataStr)
		buf := bytes.Buffer{}
		w := brotli.NewWriterV2(&buf, 4)
		n, err := w.Write(dataBytes)
		assert.Eq(t, n, len(dataBytes))
		assert.Eq(t, err, nil)
		err = w.Close()
		assert.NoErr(t, err)

		r := cbrotli.NewReader(&buf)
		var dst = []byte("")
		p := make([]byte, 100)

		for {
			n, err = r.Read(p)
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			dst = append(dst, p[:n]...)
		}
		err = r.Close()
		assert.NoErr(t, err)
		assert.Eq(t, dataBytes, dst)
	}
}
*/
