package fasthttp

import (
	"github.com/newacorn/goutils"
	"net"
	"testing"
	"time"
)

func TestAbsoluteToUnix(t *testing.T) {
	t.Parallel()
	start := time.Now()
	start2 := absoluteNano()
	time.Sleep(time.Millisecond * 500)
	a := (absoluteNano() - start2) / 1e6
	b := time.Now().Sub(start).Milliseconds()
	if a != b {
		t.Fatalf("expected %d but got %d", b, a)
	}
	c := absoluteToUTC(absoluteNano())
	d := time.Now().UTC()
	diff := c.UnixMilli() - d.UnixMilli()
	if diff > 10 || diff < -10 {
		t.Fatalf("expected %d but got %d", d.UnixMilli(), c.UnixMilli())
	}
}
func TestStruct(t *testing.T) {
	t.Skip()
	type A struct {
		a int
	}
	//http.ServeMux{}.HandleFunc(func() {})
	//a := A{a: 1}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		var c net.Conn
		c, err = ln.Accept()
		//c.Write([]byte("1"))
		c.Close()
		time.Sleep(time.Millisecond * 1000)
		_ = c
	}()
	con, err := net.Dial("tcp", ln.Addr().String())
	time.Sleep(time.Millisecond * 100)
	//v := reflect.ValueOf(con).Elem().FieldByName("fd")
	c1, ok := con.(*net.TCPConn)
	//c1.Read()
	var p [1]byte
	if ok {
		n, err := goutils.NoBlockingRead(c1, p[:])
		_ = n
		if err != nil {
			t.Log(err)
		}
		t.Log(string(p[:]))
	}
	time.Sleep(time.Millisecond * 210)
}
