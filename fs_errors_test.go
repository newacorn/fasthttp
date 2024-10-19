package fasthttp

import (
	"bufio"
	"bytes"
	"github.com/gookit/goutil/testutil/assert"
	"runtime"
	"testing"
	"time"
)

func TestFSCompressConcurrentWithError(t *testing.T) {
	// Don't run this test on Windows, the Windows GitHub actions are too slow and timeout too often.
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	// This test can't run parallel as files in / might be changed by other tests.
	stop := make(chan struct{})
	defer close(stop)

	runFSCompressConcurrentWithError(t, &FS{
		Root:               ".",
		GenerateIndexPages: true,
		Compress:           true,
		CleanStop:          stop,
	})
}

func runFSCompressConcurrentWithError(t *testing.T, fs *FS) {
	t.Helper()

	h := fs.NewRequestHandlerWithError()

	concurrency := 4
	ch := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			for j := 0; j < 5; j++ {
				testFSCompressWithError(t, h, "/fs.go")
				testFSCompressWithError(t, h, "/examples/")
				testFSCompressWithError(t, h, "/README.md")
			}
			ch <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second * 2):
			t.Fatalf("timeout")
		}
	}
}
func testFSCompressWithError(t *testing.T, h RequestHandlerWithError, filePath string) {
	t.Helper()

	// File locking is flaky on Windows.
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)

	var resp Response

	// request uncompressed file
	ctx.Request.Reset()
	ctx.Request.SetRequestURI(filePath)
	h(&ctx)
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Errorf("unexpected error: %v. filePath=%q", err, filePath)
	}
	if resp.StatusCode() != StatusOK {
		t.Errorf("unexpected status code: %d. Expecting %d. filePath=%q", resp.StatusCode(), StatusOK, filePath)
	}
	ce := resp.Header.ContentEncoding()
	if len(ce) != 0 {
		t.Errorf("unexpected content-encoding %q. Expecting empty string. filePath=%q", ce, filePath)
	}
	body := string(resp.Body())

	// request compressed gzip file
	ctx.Request.Reset()
	ctx.Request.SetRequestURI(filePath)
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip")
	h(&ctx)
	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Errorf("unexpected error: %v. filePath=%q", err, filePath)
	}
	if resp.StatusCode() != StatusOK {
		t.Errorf("unexpected status code: %d. Expecting %d. filePath=%q", resp.StatusCode(), StatusOK, filePath)
	}
	ce = resp.Header.ContentEncoding()
	if string(ce) != "gzip" {
		t.Errorf("unexpected content-encoding %q. Expecting %q. filePath=%q", ce, "gzip", filePath)
	}
	zbody, err := resp.BodyGunzip()
	if err != nil {
		t.Errorf("unexpected error when gunzipping response body: %v. filePath=%q", err, filePath)
	}
	if string(zbody) != body {
		t.Errorf("unexpected body len=%d. Expected len=%d. FilePath=%q", len(zbody), len(body), filePath)
	}

	// request compressed brotli file
	ctx.Request.Reset()
	ctx.Request.SetRequestURI(filePath)
	ctx.Request.Header.Set(HeaderAcceptEncoding, "br")
	h(&ctx)
	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err = resp.Read(br); err != nil {
		t.Errorf("unexpected error: %v. filePath=%q", err, filePath)
	}
	if resp.StatusCode() != StatusOK {
		t.Errorf("unexpected status code: %d. Expecting %d. filePath=%q", resp.StatusCode(), StatusOK, filePath)
	}
	ce = resp.Header.ContentEncoding()
	if string(ce) != "br" {
		t.Errorf("unexpected content-encoding %q. Expecting %q. filePath=%q", ce, "br", filePath)
	}
	zbody, err = resp.BodyUnbrotli()
	if err != nil {
		t.Errorf("unexpected error when unbrotling response body: %v. filePath=%q", err, filePath)
	}
	if string(zbody) != body {
		t.Errorf("unexpected body len=%d. Expected len=%d. FilePath=%q", len(zbody), len(body), filePath)
	}
}

func testPathNotFoundWithError(t *testing.T, pathNotFoundFunc RequestHandler) {
	t.Helper()

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("http//some.url/file")
	ctx.Init(&req, nil, TestLogger{t})

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		Root:         "./",
		PathNotFound: pathNotFoundFunc,
		CleanStop:    stop,
	}
	err := fs.NewRequestHandlerWithError()(&ctx)
	assert.Eq(t, err, FsFileNotFoundErr)

	/*
		if pathNotFoundFunc == nil {
			// different to ...
			if !bytes.Equal(ctx.Response.Body(),
				[]byte("Cannot open requested path")) {
				t.Fatalf("response defers. Response: %q", ctx.Response.Body())
			}
		} else {
			// Equals to ...
			if bytes.Equal(ctx.Response.Body(),
				[]byte("Cannot open requested path")) {
				t.Fatalf("response defers. Response: %q", ctx.Response.Body())
			}
		}
	*/
}

func TestPathNotFoundWithError(t *testing.T) {
	t.Parallel()

	testPathNotFoundWithError(t, nil)
}

func TestServeFileUncompressedWithError(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("http://foobar.com/baz")
	req.Header.Set(HeaderAcceptEncoding, "gzip")
	ctx.Init(&req, nil, nil)

	ServeFileUncompressed(&ctx, "fs.go")

	var resp Response
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ce := resp.Header.ContentEncoding()
	if len(ce) > 0 {
		t.Fatalf("Unexpected 'Content-Encoding' %q", ce)
	}

	body := resp.Body()
	expectedBody, err := getFileContents("/fs.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body %q. expecting %q", body, expectedBody)
	}
}
