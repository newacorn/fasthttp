package fasthttp

import (
	gbufio "bufio"
	gbytes "bytes"
	"compress/gzip"
	"fmt"
	"github.com/andybalholm/brotli"
	"github.com/gookit/goutil/testutil/assert"
	"github.com/newacorn/goutils/bytes"
	bpool "github.com/newacorn/simple-bytes-pool"
	"github.com/xyproto/randomstring"
	"io"
	"log"
	"strconv"
	"testing"
	"utils/compress"
)

func BenchmarkBodyCompress(b *testing.B) {
	var dataStr = randomstring.HumanFriendlyString(1200)
	var dataBytes = []byte(dataStr)
	resp := AcquireResponse()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp.SetBodyRaw(dataBytes)
		bodyBytes := resp.bodyBytes()
		w := responseBodyPool.Get()
		w.B = AppendBrotliBytesLevel(w.B, bodyBytes, CompressBrotliDefaultCompression)

		// Hack: swap resp.body with w.
		if resp.body != nil {
			responseBodyPool.Put(resp.body)
		}
		//log.Println(string(w.B))
		resp.body = w
		resp.bodyRaw = nil
	}

}

func brotliReaderFastHttpImpl(r io.Reader, level int) io.Reader {
	return NewStreamReader(func(sw *gbufio.Writer) {
		zw := acquireStacklessBrotliWriter(sw, level)
		fw := &flushWriter{
			wf: zw,
			bw: sw,
		}
		_, wErr := copyZeroAlloc(fw, r)
		releaseStacklessBrotliWriter(zw, level)
		switch v := r.(type) {
		case io.Closer:
			_ = v.Close()
		case ReadCloserWithError:

			_ = v.CloseWithError(wErr) //nolint:errcheck
		}
	})
}
func BenchmarkBrotliStreamCompressFastHttp(b *testing.B) {
	var dataStr = randomstring.HumanFriendlyString(1200)
	var dataBytes = []byte(dataStr)
	buf := bytes.Buffer{}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = buf.Write(dataBytes)
		s := brotliReaderFastHttpImpl(&buf, 4)

		//
		py := bpool.Get(len(dataBytes))
		py.B = py.B[:len(dataBytes)]
		r := compress.DefaultBrotliReaderPool.Get()
		r.Reset(s)
		n2, err := r.Read(py.B)
		if err != nil && err != io.EOF {
			b.Fatal(err)
		}
		gbytes.Equal(py.B[:n2], dataBytes)
		//r.NilSrc()
		compress.DefaultBrotliReaderPool.Put(r)
		py.RecycleToPool00()
		buf.Reset()
	}
}

// func BenchmarkParallel() {}
func TestBrotliWriterPoolAndReaderPool(t *testing.T) {
	t.Skip()
	var dataBytesSlice [][]byte
	i := 1
	k := 16
	for {
		dataBytesSlice = append(dataBytesSlice, []byte(randomstring.HumanFriendlyString(i<<k)))
		k++
		if k == 17 {
			break
		}
	}

	for _, dataBytes := range dataBytesSlice {
		buf := bytes.NewBufferSizeNoPtr(len(dataBytes))
		po := compress.DefaultBrotliCompressPools.Pool(4)
		w := po.Get()
		w.Reset(&buf)
		//
		n1, err := w.Write(dataBytes)
		if err != nil {
			t.Fatal(err, n1)
		}
		_ = w.Close()
		//BrotliWriterPools[4].Put(w)
		po.Put(w)
		//
		r := compress.DefaultBrotliReaderPool.Get()
		_ = r.Reset(&buf)
		py := bpool.Get(len(dataBytes) + 10)
		py.B = py.B[:len(dataBytes)+10]
		p := py.B
		total := 0
		for {
			n, err := r.Read(p)

			if err != nil {
				if err != io.EOF {
					t.Fatal(err)
				}
				break
			}
			total += n
			p = p[n:]
		}
		if !gbytes.Equal(py.B[:total], dataBytes) {
			t.Fatal("not equal")
		}
		//r.NilSrc()
		compress.DefaultBrotliReaderPool.Put(r)
		py.RecycleToPool00()
		buf.RecycleItems()
	}
}

func TestBrotliEmptyData(t *testing.T) {
	fmt.Printf("%#x\n", 3)
	fmt.Printf("%#x\n", 59)
	buf := bytes.Buffer{}
	//w := brotli.NewWriterLevel(&buf, 4)
	w := brotli.NewWriterV2(&buf, 4)
	w.Write(nil)
	w.Close()
	r := brotli.NewReader(&buf)
	m := make([]byte, 1000)
	n, err := r.Read(m)
	n, err = r.Read(m)
	assert.Eq(t, 0, n)
	assert.Eq(t, io.EOF, err)
}
func TestBrotliWriteMultiplyBlockData(t *testing.T) {
	tmpW := brotli.NewWriterV2(nil, 4)
	blockSize := tmpW.BlockSize
	for i := 0; i < 5; i++ {
		t.Run("compress data length: "+strconv.Itoa(i*blockSize)+"bytes", func(t *testing.T) {
			dataStr := randomstring.HumanFriendlyString(i * blockSize)
			dataBytes := []byte(dataStr)
			buf := bytes.Buffer{}
			w := brotli.NewWriterV2(&buf, 4)
			n, err := w.Write(dataBytes)
			err = w.Close()
			assert.NoErr(t, err)
			assert.Eq(t, n, len(dataBytes))
			assert.NoErr(t, err)
			r := brotli.NewReader(&buf)
			dst := make([]byte, len(dataBytes)+1000)
			p := dst
			total := 0
			for {
				n1, err1 := r.Read(p)
				if err1 != nil {
					if err1 != io.EOF {
						t.Fatal(err1)
					}
					break
				}
				total += n1
				p = p[n1:]
			}
			assert.Eq(t, dataBytes, dst[:total])
		})
	}

}
func TestBrotliNewWriterV2Simple(t *testing.T) {
	for i := 65536; i <= 65536*4; i++ {
		if i%65536 != 0 {
			continue
		}
		t.Run("compress data length: "+strconv.Itoa(i)+"bytes", func(t *testing.T) {
			dataStr := randomstring.HumanFriendlyString(i)
			dataBytes := []byte(dataStr)
			buf := bytes.Buffer{}
			w := brotli.NewWriterV2(&buf, 4)

			n, err := w.Write(dataBytes)
			assert.NoErr(t, err)
			assert.Eq(t, n, len(dataBytes))
			err = w.Close()
			assert.NoErr(t, err)

			r := brotli.NewReader(&buf)
			dst := make([]byte, len(dataBytes)+100)
			//p := make([]byte, 200)
			p := dst
			total := 0
			for {
				n1, err1 := r.Read(p)
				if err1 != nil {
					if err1 != io.EOF {
						//t.Fatal(err1)
						break
					}
					break
				}
				p = p[n1:]
				//dst = append(dst, p[:n1]...)
				total += n1
			}
			//assert.Eq(t, dst[:total], dataBytes)

		})

	}
}

func TestBrotliWriterPoolAndReaderPoolFix(t *testing.T) {
	var dataBytesSlice [][]byte
	i := 1
	k := 16
	for {
		dataBytesSlice = append(dataBytesSlice, []byte(randomstring.HumanFriendlyString(i<<k)))
		k++
		if k == 17 {
			break
		}
	}

	for _, dataBytes := range dataBytesSlice {
		//buf := bytes.NewBufferSizeNoPtr(len(dataBytes))
		buf := bytes.Buffer{}
		w := brotli.NewWriterV2(&buf, 4)
		w.Reset(&buf)
		//
		n1, err := w.Write(dataBytes)
		if err != nil {
			t.Fatal(err, n1)
		}
		_ = w.Close()
		r := brotli.NewReader(&buf)
		//
		dst := make([]byte, len(dataBytes)+100)
		p := dst
		total := 0
		for {
			n, err := r.Read(p)

			if err != nil {
				if err != io.EOF {
					//t.Fatal(err) // when n = 65536
					log.Println(err, len(dataBytes))
					//t.Error(err)
					break
				}
				break
			}
			total += n
			p = p[n:]
		}
		if !gbytes.Equal(dst[:total], dataBytes) {
			//log.Println(len(dataBytes))
			log.Println("not equal.", len(dataBytes))
			//t.Fatal("not equal")
		}
		//r.NilSrc()
	}
}

/*
	func TestStdIoReader(t *testing.T) {
		r := strings.NewReader("")
		p := make([]byte, 100)
		_ = p
		n, err := r.Read(nil)
		assert.NoErr(t, err)
		assert.Eq(t, 4, n)
		n, err = r.Read(nil)
		assert.Eq(t, io.EOF, err)
		assert.Eq(t, 0, n)
	}
*/
func TestBrotliReaderEmptyData(t *testing.T) {
	readPLen := [][]int{{0, 1, 0, 2, 4}, {2, 3, 0, 4}}
	for _, pLen := range readPLen {
		tmp := pLen
		t.Run("read p len order: "+fmt.Sprintf("%v", pLen), func(t *testing.T) {
			buf := bytes.Buffer{}
			w := brotli.NewWriterV2(&buf, 4)
			err := w.Close()
			if err != nil {
				t.Fatal(err)
			}
			r := brotli.NewReader(&buf)
			for _, l1 := range tmp {
				p := make([]byte, l1)
				n1, err1 := r.Read(p)
				if n1 != 0 {
					t.Fatalf("expected %d but got %d", 0, n1)
				}
				if err1 != io.EOF {
					t.Fatal(err1)
				}
			}
		})

	}
}

func TestGzipReaderEmptyData(t *testing.T) {
	readPLen := [][]int{{0, 1, 0, 2, 4}, {2, 3, 0, 4}}
	for _, pLen := range readPLen {
		tmp := pLen
		t.Run("read p len order: "+fmt.Sprintf("%v", pLen), func(t *testing.T) {
			buf := bytes.Buffer{}
			w, _ := gzip.NewWriterLevel(&buf, 4)
			err := w.Close()
			if err != nil {
				t.Fatal(err)
			}
			r, _ := gzip.NewReader(&buf)
			for _, l1 := range tmp {
				p := make([]byte, l1)
				n1, err1 := r.Read(p)
				if n1 != 0 {
					t.Fatalf("expected %d but got %d", 0, n1)
				}
				if err1 != io.EOF {
					t.Fatal(err1)
				}
			}
		})

	}
}
