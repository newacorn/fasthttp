package fasthttp

import (
	"bytes"
	"errors"
	"github.com/gookit/goutil/testutil/assert"
	"github.com/valyala/fasthttp"
	"golang.org/x/net/proxy"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
	"utils/compress"
)

type ProxyTCPDialer struct {
	TCPDialer
}

func (c *ProxyTCPDialer) Dial(network, addr string) (net.Conn, error) {
	if network == "tcp4" {
		return c.TCPDialer.Dial(addr)
	}
	if network == "tcp" {
		return c.TCPDialer.DialDualStack(addr)

	}
	return nil, errors.New("don't know how to dial network:" + network)
}
func (c *ProxyTCPDialer) GetDialFunc(proxyAddr string) (dialFunc DialFunc, err error) {
	u, err := url.Parse(proxyAddr)
	if err != nil {
		return
	}
	dialer, err := proxy.FromURL(u, c)
	if err != nil {
		return
	}
	dialFunc = func(addr string) (net.Conn, error) {
		return dialer.Dial("tcp", addr)
	}
	return
}

//

func handleCon(c net.Conn) {
	_, err := c.Write([]byte("hello world"))
	if err != nil {
		log.Println(err)
	}
	time.Sleep(time.Second * 1000)
}
func handleHttpCon(c net.Conn) {
	resp := AcquireResponse()
	resp.AppendBody([]byte("hello world"))
	_, err := resp.WriteTo(c)
	if err != nil {
		log.Println(err)
	}
	time.Sleep(time.Second * 1000)
}

var fistReqSuccessCount atomic.Int64

func init() {
	log.SetFlags(log.Lshortfile)
}

/*
	func TestSeverSetReadDeadlineUseHeaderReceivedBug(t *testing.T) {
		const clientConnCount = 500
		var cliConnSecReqNotify = make(chan struct{})
		s := fasthttp.Server{HeaderReceived: func(header *fasthttp.RequestHeader) fasthttp.RequestConfig {
			return fasthttp.RequestConfig{ReadTimeout: time.Hour}
		}}
		go func() {
			s.Handler = func(ctx *fasthttp.RequestCtx) {
				defer func() {
					if recover() != nil {
						t.Error(recover())
					}
				}()
				_, _ = ctx.WriteString("hello world")
			}
			err := s.ListenAndServe(":7070")
			if err != nil {
				log.Println(err)
			}
			time.Sleep(time.Hour)
		}()
		time.Sleep(time.Millisecond * 100)
		wg := sync.WaitGroup{}
		fistRqSend := sync.WaitGroup{}
		for i := 0; i < clientConnCount; i++ {
			wg.Add(1)
			fistRqSend.Add(1)
			time.Sleep(time.Millisecond * 10)
			go func() {
				defer wg.Done()
				cliCon, err := net.Dial("tcp", ":7070")
				if err != nil {
					log.Println(err)
					fistRqSend.Done()
					return
				}
				httpReqStr := "GET / HTTP/1.1\r\n\r\n"
				_, err = cliCon.Write([]byte(httpReqStr))
				if err != nil {
					log.Println(err)
				}
				resp := AcquireResponse()
				defer func() { ReleaseResponse(resp) }()
				buf := bufio.NewReader(cliCon)
				err = resp.Read(buf)
				if err != nil {
					log.Println(err)
				}
				assert.Eq(t, resp.Body(), []byte("hello world"))
				fistRqSend.Done()
				fistReqSuccessCount.Add(1)
				<-cliConnSecReqNotify
				cliCon.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
				cliCon.Write([]byte("\r\n"))
				buf.Reset(cliCon)
				resp.Reset()
				resp.Read(buf)
			}()
		}
		fistRqSend.Wait()
		log.Println(fistReqSuccessCount.Load())
		close(cliConnSecReqNotify)
		time.Sleep(time.Millisecond)
		s.Shutdown()
		wg.Wait()
	}
*/

func TestBC(t *testing.T) {
	t.Skip()
	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	go func() {
		con, err := ln.Accept()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
			return
		}
		_ = con

	}()
	con, err := net.Dial("tcp4", ":8080")
	if err != nil {
		log.Fatal(err)
	}
	err = con.SetWriteDeadline(time.Now().Add(time.Second))
	time.Sleep(time.Second * 2)
	con.SetWriteDeadline(time.Now().Add(time.Second))
	log.Println(con.Write([]byte("hello")))
}

func TestD(t *testing.T) {
	t.Skip()
	mu := http.DefaultServeMux
	mu.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("A", "B")
		w.Header().Add("Trailer", "C")
		w.Header().Add("Trailer", "B")
		w.Write([]byte("hello"))
		w.Header().Set("B", "C")
		w.Header().Set("C", "D")
	})
	go func() {
		http.ListenAndServe("127.0.0.1:8089", nil)
	}()
	time.Sleep(time.Millisecond * 100)
	time.Sleep(time.Hour)
}
func TestE(t *testing.T) {
	ln, err := net.Listen("tcp4", ":8089")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			con, err := ln.Accept()
			if err != nil {
				t.Fatal(err)
			}
			con.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		}

	}()
	resp, err := http.Get("http://127.0.0.1:8089")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(resp.Header.Get("Connection"))
}
func TestF(t *testing.T) {
	t.Skip()
	ln, err := net.Listen("tcp4", "127.0.0.1:8089")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		con, err := ln.Accept()
		if err != nil {
			//t.Fatal(err)
			t.Error(err)
			return
		}
		log.Println(33)
		time.Sleep(time.Second * 10)
		t1 := con.(*net.TCPConn)
		log.Println(t1.SetKeepAlive(true))
		log.Println(t1.SetKeepAlivePeriod(time.Second * 10))
		_ = con
	}()

	time.Sleep(time.Second * 1)
	c, err := net.Dial("tcp4", "127.0.0.1:8089")
	c.Close()
	time.Sleep(time.Second * 10000)
	request := &fasthttp.Request{}
	request.SetRequestURI("http://example.com/test-1")

	println(string(request.Host()))
	println(string(request.RequestURI()))
	println(string(request.Host()))
	/*
		mu := http.DefaultServeMux
		mu.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
			writer.Write()
			writer.Flush()
		})
	*/
	//Response{}.Header.Set()
	//Request{}.H
	//http.Request{}.AddCookie()
	//http.Request{}.Header.Set()
}
func TestChunkWriter(t *testing.T) {
	t.Skip()
	err := ListenAndServe("127.0.0.1:8089", func(ctx *RequestCtx) {
		//compress.Debug = true
		//ctx.Response.compressPool = compress.DefaultCBrotliCompressPools.Pool(-1)
		//ctx.Response.Header.SetContentEncoding("br")
		//
		ctx.Response.compressPool = compress.DefaultGzipCompressPools.Pool(-1)
		ctx.Response.Header.SetContentEncoding("gzip")
		ctx.w = func(w WriterFlusherCloser) (err error) {
			_, err = w.Write([]byte("hello"))
			if err != nil {
				return
			}
			//err = w.Flush()
			//if err != nil {
			//	return
			//}
			_, err = w.Write(bytes.Repeat([]byte("hello"), 1000))
			if err != nil {
				return
			}
			err = w.Flush()
			if err != nil {
				return
			}
			_, err = w.Write(bytes.Repeat([]byte("hello"), 1000))
			if err != nil {
				return
			}
			err = w.Flush()
			if err != nil {
				return
			}
			//err = w.Flush()
			return
		}
	})
	assert.Nil(t, err)
}
