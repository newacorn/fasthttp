package fasthttp

import (
	"bufio"
	"github.com/gookit/goutil/testutil/assert"
	"github.com/valyala/fasthttp/fasthttputil"
	"io"
	"os"
	"strings"
	"testing"
)

var curDir string

func init() {
	curDir, _ = os.Getwd()
}

// 可以决定是否丢弃Request.bodyStream中的未读的数据，以免影响下一次请求的解析。
// 通过Request的Body方法返回的字节切片应当尊重Server的MaxRequestBodySize设置。
func TestRequestBodyStreamConvertByteSlice(t *testing.T) {
	const maxReqBodySize = 1024
	f, err := os.Open(curDir + "/testdata/vue.runtime.global.js")
	assert.NoErr(t, err)
	fSize, err := f.Seek(0, io.SeekEnd)
	assert.NoErr(t, err)
	_, err = f.Seek(0, io.SeekStart)
	assert.NoErr(t, err)
	//
	pcs := fasthttputil.NewPipeConns()
	cliCon, serCon := pcs.Conn1(), pcs.Conn2()
	req := AcquireRequest()
	req.SetBodyStream(f, fSize)
	req.SetRequestURI("http://localhost:8080")
	respBody := []byte("Hello World!")
	go func() {
		_, err = req.WriteTo(cliCon)
		assert.NoErr(t, err)
		resp := AcquireResponse()
		err = resp.Read(bufio.NewReader(cliCon))
		assert.NoErr(t, err)
		assert.Eq(t, respBody, resp.Body())
		err = cliCon.Close()
		assert.NoErr(t, err)
	}()
	//
	server := Server{StreamRequestBody: true, MaxRequestBodySize: maxReqBodySize,
		DiscardUnReadRequestBodyStream: true,
		Handler: func(ctx *RequestCtx) {
			_, _ = ctx.Write(respBody)
			assert.Eq(t, maxReqBodySize, len(ctx.Request.Body()))
		}}
	err = server.ServeConn(serCon)
	if err != nil {
		t.Fatal(err)
	}
}
func TestRepeatSetContentLength(t *testing.T) {
	const reqBodyLength = 100
	req := AcquireRequest()
	req.SetRequestURI("http://localhost:8080")
	req.Header.SetMethod("POST")
	req.SetBodyStream(strings.NewReader(strings.Repeat("a", reqBodyLength)), -1)
	server := Server{
		Handler: func(ctx *RequestCtx) {
			assert.Eq(t, int64(-1), ctx.Request.Header.ContentLength())
		},
		StreamRequestBody:              true,
		DiscardUnReadRequestBodyStream: true,
	}
	cliServerHelper(t, req, &server)
}
func cliServerHelper(t *testing.T, req *Request, server *Server, resps ...*Response) {
	pcs := fasthttputil.NewPipeConns()
	cliCon, serCon := pcs.Conn1(), pcs.Conn2()
	go func() {
		_, err := req.WriteTo(cliCon)
		assert.NoErr(t, err)
		var resp *Response
		if len(resps) > 0 {
			resp = resps[0]
		} else {
			resp = AcquireResponse()
			defer func() {
				ReleaseResponse(resp)
			}()
		}
		t.Log("close...1")
		err = resp.Read(bufio.NewReader(cliCon))
		//resp.SetBodyStream()
		assert.NoErr(t, err)
		t.Log("close...")
		err = cliCon.Close()
		t.Log("close")
		assert.NoErr(t, err)
	}()
	//
	err := server.ServeConn(serCon)
	if err != nil {
		t.Fatal(err)
	}
}
func TestContentLengthBigOrEqualZero(t *testing.T) {
	req := AcquireRequest()
	req.SetBody([]byte("Hello World!"))
	req.SetRequestURI("http://localhost:8080")
	cliServerHelper(t, req, &Server{Handler: func(ctx *RequestCtx) {
		ctx.WriteString("Ok")
	}, StreamRequestBody: true})
}

/*
func TestConnsMap(t *testing.T) {
	server := Server{
		Handler: func(ctx *RequestCtx) {
			_, _ = ctx.WriteString("Hello World!")
		},
		ShutDownV2: true,
	}
	go func() {
		time.Sleep(time.Millisecond * 500)
		for {
			server.conns.Range(func(key net.Conn, value *ConnStatus) bool {
				t.Logf("remote addr: %s;createAt %s;lastActiveAt %s", key.RemoteAddr().String(), value.createTime.Format("2006-01-02 15:04:05"), time.Unix(int64(value.lastActive.Load()), 0).Format("2006-01-02 15:04:05"))
				return true
			})
			time.Sleep(time.Second * 10)
		}
	}()
	err := server.ListenAndServe(":7070")
	assert.NoErr(t, err)
}
*/
