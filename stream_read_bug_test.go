package fasthttp

import (
	"bufio"
	"bytes"
	"github.com/valyala/fasthttp/fasthttputil"
	"io"
	"testing"
)

func TestStreamReadBug(t *testing.T) {
	ps := fasthttputil.NewPipeConns()
	cliCon, serverCon := ps.Conn1(), ps.Conn2()

	go func() {
		req := AcquireRequest()
		defer ReleaseRequest(req)
		req.Header.SetContentLength(10)
		req.Header.SetMethod("POST")
		req.SetRequestURI("http://localhsot:8080")
		req.SetBodyRaw(bytes.Repeat([]byte{'1'}, 10))
		//
		var pipelineReqBody []byte
		reqBody := req.String()
		pipelineReqBody = append(pipelineReqBody, reqBody...)
		pipelineReqBody = append(pipelineReqBody, reqBody...)
		/*
			reqBody := strings.Replace(req.String(), "20", "10", 1)
		*/
		//req.Write()
		//reqBody := req.String()
		//println(reqBoyd)
		_, err := cliCon.Write(pipelineReqBody)
		if err != nil {
			t.Error(err)
		}
		resp := AcquireResponse()
		err = resp.Read(bufio.NewReader(cliCon))
		if err != nil {
			t.Error(err)
		}
		err = cliCon.Close()
		if err != nil {
			t.Error(err)
		}
	}()
	//
	server := Server{StreamRequestBody: true, MaxRequestBodySize: 5, Handler: func(ctx *RequestCtx) {
		//dst := make([]byte, 100)
		r := ctx.RequestBodyStream()
		p := make([]byte, 1300)
		for {
			_, err := r.Read(p)
			if err != nil {
				if err != io.EOF {
					t.Fatal(err)
				}
				break
			}
		}
	}}
	err := server.ServeConn(serverCon)
	if err != nil {
		t.Fatal(err)
	}
}
