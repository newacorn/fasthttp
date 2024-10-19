package fasthttp

import (
	"bytes"
	"github.com/gookit/goutil/testutil/assert"
	pio "github.com/newacorn/goutils/io"
	"log"
	"net"
	"sync"
	"testing"
	"time"
)

func TestBufferB(t *testing.T) {
	t.Skip()
	l := NewLinkBuffer()
	l.WriteBinary(bytes.Repeat([]byte("1"), 8192*10))
	l.Flush()
	l.GetBytes(nil)
	l.Skip(8192 * 10)
	//l.Release()
	l.WriteBinary(bytes.Repeat([]byte("2"), 4096))
	//l.MallocAck(0)
	l.Flush()
	log.Println(l.Len())
}
func TestBufferA(t *testing.T) {
	t.Skip()
	l := NewLinkBuffer()
	log.Println(l.WriteBinary(bytes.Repeat([]byte("1"), 8192)))
	err := l.WriteDirect(bytes.Repeat([]byte("2"), 1024), 768)
	assert.NoError(t, err)
	/*
		n, err := l.WriteBinary(bytes.Repeat([]byte("1"), 8192))
		assert.NoErr(t, err)
		l.MallocAck(10)
		l.Flush()
	*/
	log.Println(l.Len())
	log.Println(l.Flush())
	log.Println(l.Len())

	for _, v := range l.GetBytes(nil) {
		log.Println(len(v))
	}
	//assert.Equal(t, 8192, n)
	//log.Println(l.ReadBinary(10))
	//b := net.Buffers{}
	//b.WriteTo()

}

func BenchmarkBufferWriter(b *testing.B) {
	b.Skip()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatalln(err)
	}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer func() {
			wg.Done()
		}()
		con, err := l.Accept()
		if err != nil {
			return
		}
		p, _ := pio.Discard.(pio.DiscardWithEOF)
		for {
			n, err := p.ReadFromWithEOF(con)
			if err != nil {
				//return
				break
			}
			log.Println(n)
		}
	}()
	addr := l.Addr().String()
	time.Sleep(time.Millisecond * 10)
	cliCon, err := net.Dial("tcp", addr)
	log.Println(err)
	data := bytes.Repeat([]byte("1"), 1024)
	l2 := NewLinkBuffer()
	_ = cliCon
	//dst := &bytes.Buffer{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l2.WriteBinary(data)
		if i%30 == 0 {
			l2.Flush()
			//log.Println(l2.Len())
			vs := l2.GetBytes(nil)
			bu := net.Buffers(vs)
			//n2, err := bu.WriteTo(cliCon)
			n2, err := bu.WriteTo(cliCon)
			if err != nil {
				b.Fatal(err)
			}
			//log.Println(n2, err)
			l2.Skip(int(n2))
			l2.Release()
		}
	}
	cliCon.Close()
	l.Close()
	wg.Wait()
	b.Log(l2.memorySize())
	//l2.pee
}

func BenchmarkBufferWriterV2(b *testing.B) {
	l, err := net.Listen("tcp", ":0")
	_ = err
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer func() {
			wg.Done()
		}()
		con, err := l.Accept()
		if err != nil {
			b.Fatal(err)
		}
		p, _ := pio.Discard.(pio.DiscardWithEOF)
		for {
			n, err := p.ReadFromWithEOF(con)
			if err != nil {
				return
			}
			_ = n
			//io.Copy(pio.Discard, con)
		}
	}()
	addr := l.Addr()
	time.Sleep(time.Millisecond * 100)
	cliCon, _ := net.Dial("tcp", addr.String())
	data := bytes.Repeat([]byte("1"), 1024)
	buf := responseBodyPool.Get()
	//
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Write(data)
		if i%30 == 0 {
			l1 := int64(buf.Len())
			for {
				n1, err := buf.WriteTo(cliCon)
				if err != nil {
					b.Fatal(err)
				}
				l1 -= n1
				if l1 == 0 {
					break
				}
			}
			buf.Reset()
		}
	}
	cliCon.Close()
	l.Close()
	wg.Wait()
}
