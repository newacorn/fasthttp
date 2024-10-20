package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/gookit/goutil/testutil/assert"
	"github.com/newacorn/goutils/compress"
	"github.com/puzpuzpuz/xsync/v3"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type TestLogger struct {
	t *testing.T
}

func (t TestLogger) Printf(format string, args ...any) {
	t.t.Logf(format, args...)
}
func (t TestLogger) Write(b []byte) {
	t.t.Logf(b2s(b))
}

func TestNewVHostPathRewriter(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.Header.SetHost("foobar.com")
	req.SetRequestURI("/foo/bar/baz")
	ctx.Init(&req, nil, nil)

	f := NewVHostPathRewriter(0)
	path := f(&ctx)
	expectedPath := "/foobar.com/foo/bar/baz"
	if string(path) != expectedPath {
		t.Fatalf("unexpected path %q. Expecting %q", path, expectedPath)
	}

	ctx.Request.Reset()
	ctx.Request.SetRequestURI("https://aaa.bbb.cc/one/two/three/four?asdf=dsf")
	f = NewVHostPathRewriter(2)
	path = f(&ctx)
	expectedPath = "/aaa.bbb.cc/three/four"
	if string(path) != expectedPath {
		t.Fatalf("unexpected path %q. Expecting %q", path, expectedPath)
	}
}

func TestNewVHostPathRewriterMaliciousHost(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.Header.SetHost("/../../../etc/passwd")
	req.SetRequestURI("/foo/bar/baz")
	ctx.Init(&req, nil, nil)

	f := NewVHostPathRewriter(0)
	path := f(&ctx)
	expectedPath := "/invalid-host/"
	if string(path) != expectedPath {
		t.Fatalf("unexpected path %q. Expecting %q", path, expectedPath)
	}
}

func testPathNotFound(t *testing.T, pathNotFoundFunc RequestHandler) {
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
	fs.NewRequestHandler()(&ctx)

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
}

func TestPathNotFound(t *testing.T) {
	t.Parallel()

	testPathNotFound(t, nil)
}

func TestPathNotFoundFunc(t *testing.T) {
	t.Parallel()

	testPathNotFound(t, func(ctx *RequestCtx) {
		ctx.WriteString("Not found hehe") //nolint:errcheck
	})
}

func TestServeFileHead(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	var ctx RequestCtx
	var req Request
	req.Header.SetMethod(MethodHead)
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFile(&ctx, "fs.go")

	var resp Response
	resp.SkipBody = true
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
	if len(body) > 0 {
		t.Fatalf("unexpected response body %q. Expecting empty body", body)
	}

	expectedBody, err := getFileContents("/fs.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	contentLength := resp.Header.ContentLength()
	if contentLength != int64(len(expectedBody)) {
		t.Fatalf("unexpected Content-Length: %d. expecting %d", contentLength, len(expectedBody))
	}
}

func TestServeFileSmallNoReadFrom(t *testing.T) {
	t.Parallel()

	expectedStr := "hello, world!"
	tempFile := filepath.Join(t.TempDir(), "hello")

	if err := os.WriteFile(tempFile, []byte(expectedStr), 0o666); err != nil {
		t.Fatal(err)
	}

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFile(&ctx, tempFile)

	reader, ok := ctx.Response.bodyStream.(*fsSmallFileReader)
	if !ok {
		t.Fatal("expected fsSmallFileReader")
	}
	//defer reader.ff.Release()

	buf := bytes.NewBuffer(nil)

	n, err := reader.WriteTo(pureWriter{buf})
	if err != nil {
		t.Fatal(err)
	}

	if n != int64(len(expectedStr)) {
		t.Fatalf("expected %d bytes, got %d bytes", len(expectedStr), n)
	}

	body := buf.String()
	if body != expectedStr {
		t.Fatalf("expected '%q'", expectedStr)
	}
}

type pureWriter struct {
	w io.Writer
}

func (pw pureWriter) Write(p []byte) (nn int, err error) {
	return pw.w.Write(p)
}

func TestServeFileCompressed(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)

	var resp Response

	// request compressed gzip file
	ctx.Request.SetRequestURI("http://foobar.com/baz")
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip")
	ServeFile(&ctx, "fs.go")

	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ce := resp.Header.ContentEncoding()
	if string(ce) != "gzip" {
		t.Fatalf("Unexpected 'Content-Encoding' %q. Expecting %q", ce, "gzip")
	}

	body, err := resp.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedBody, err := getFileContents("/fs.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body %q. expecting %q", body, expectedBody)
	}

	// request compressed brotli file
	ctx.Request.Reset()
	ctx.Request.SetRequestURI("http://foobar.com/baz")
	ctx.Request.Header.Set(HeaderAcceptEncoding, "br")
	ServeFile(&ctx, "fs.go")

	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err = resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ce = resp.Header.ContentEncoding()
	if string(ce) != "br" {
		t.Fatalf("Unexpected 'Content-Encoding' %q. Expecting %q", ce, "br")
	}

	body, err = resp.BodyUnbrotli()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedBody, err = getFileContents("/fs.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body %q. expecting %q", body, expectedBody)
	}
}

func TestServeFileUncompressed(t *testing.T) {
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

func TestFSByteRangeConcurrent(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSByteRangeConcurrent(t, &FS{
		Root:            ".",
		AcceptByteRange: true,
		CleanStop:       stop,
	})
}

func TestFSByteRangeConcurrentSkipCache(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSByteRangeConcurrent(t, &FS{
		Root:            ".",
		SkipCache:       true,
		AcceptByteRange: true,
		CleanStop:       stop,
	})
}

func runFSByteRangeConcurrent(t *testing.T, fs *FS) {
	t.Helper()

	h := fs.NewRequestHandler()

	concurrency := 10
	ch := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			for j := 0; j < 5; j++ {
				testFSByteRange(t, h, "/fs.go")
				testFSByteRange(t, h, "/README.md")
			}
			ch <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-time.After(time.Second * 1000):
			t.Fatalf("timeout")
		case <-ch:
		}
	}
}

func TestFSByteRangeSingleThread(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSByteRangeSingleThread(t, &FS{
		Root:            ".",
		AcceptByteRange: true,
		CleanStop:       stop,
	})
}

func TestFSByteRangeSingleThreadSkipCache(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSByteRangeSingleThread(t, &FS{
		Root:            ".",
		AcceptByteRange: true,
		SkipCache:       true,
		CleanStop:       stop,
	})
}

func runFSByteRangeSingleThread(t *testing.T, fs *FS) {
	t.Helper()

	h := fs.NewRequestHandler()

	testFSByteRange(t, h, "/fs.go")
	testFSByteRange(t, h, "/README.md")
}

func testFSByteRange(t *testing.T, h RequestHandler, filePath string) {
	t.Helper()

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)

	expectedBody, err := getFileContents(filePath)
	if err != nil {
		t.Fatalf("cannot read file %q: %v", filePath, err)
	}

	fileSize := len(expectedBody)
	startPos := int64(rand.Intn(fileSize))
	endPos := int64(rand.Intn(fileSize))
	if endPos < startPos {
		startPos, endPos = endPos, startPos
	}

	ctx.Request.SetRequestURI(filePath)
	ctx.Request.Header.SetByteRange(startPos, endPos)
	h(&ctx)

	var resp Response
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v. filePath=%q", err, filePath)
	}
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d. filePath=%q", resp.StatusCode(), StatusPartialContent, filePath)
	}
	cr := resp.Header.Peek(HeaderContentRange)

	expectedCR := fmt.Sprintf("bytes %d-%d/%d", startPos, endPos, fileSize)
	if string(cr) != expectedCR {
		t.Fatalf("unexpected content-range %q. Expecting %q. filePath=%q", cr, expectedCR, filePath)
	}
	body := resp.Body()
	bodySize := endPos - startPos + 1
	if int64(len(body)) != bodySize {
		t.Fatalf("unexpected body size %d. Expecting %d. filePath=%q, startPos=%d, endPos=%d",
			len(body), bodySize, filePath, startPos, endPos)
	}

	expectedBody = expectedBody[startPos : endPos+1]
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body %q. Expecting %q. filePath=%q, startPos=%d, endPos=%d",
			body, expectedBody, filePath, startPos, endPos)
	}
}

func getFileContents(path string) ([]byte, error) {
	path = "." + path
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func TestParseByteRangeSuccess(t *testing.T) {
	t.Parallel()

	testParseByteRangeSuccess(t, "bytes=0-0", 1, 0, 0)
	testParseByteRangeSuccess(t, "bytes=1234-6789", 6790, 1234, 6789)

	testParseByteRangeSuccess(t, "bytes=123-", 456, 123, 455)
	testParseByteRangeSuccess(t, "bytes=-1", 1, 0, 0)
	testParseByteRangeSuccess(t, "bytes=-123", 456, 333, 455)

	// End position exceeding content-length. It should be updated to content-length-1.
	// See https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35
	testParseByteRangeSuccess(t, "bytes=1-2345", 234, 1, 233)
	testParseByteRangeSuccess(t, "bytes=0-2345", 2345, 0, 2344)

	// Start position overflow. Whole range must be returned.
	// See https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35
	testParseByteRangeSuccess(t, "bytes=-567", 56, 0, 55)
}

func testParseByteRangeSuccess(t *testing.T, v string, contentLength, startPos, endPos int) {
	t.Helper()

	startPos1, endPos1, err := ParseByteRange([]byte(v), int64(contentLength))
	if err != nil {
		t.Fatalf("unexpected error: %v. v=%q, contentLength=%d", err, v, contentLength)
	}
	if startPos1 != int64(startPos) {
		t.Fatalf("unexpected startPos=%d. Expecting %d. v=%q, contentLength=%d", startPos1, startPos, v, contentLength)
	}
	if endPos1 != int64(endPos) {
		t.Fatalf("unexpected endPos=%d. Expecting %d. v=%q, contentLength=%d", endPos1, endPos, v, contentLength)
	}
}

func TestParseByteRangeError(t *testing.T) {
	t.Parallel()

	// invalid value
	testParseByteRangeError(t, "asdfasdfas", 1234)

	// invalid units
	testParseByteRangeError(t, "foobar=1-34", 600)

	// missing '-'
	testParseByteRangeError(t, "bytes=1234", 1235)

	// non-numeric range
	testParseByteRangeError(t, "bytes=foobar", 123)
	testParseByteRangeError(t, "bytes=1-foobar", 123)
	testParseByteRangeError(t, "bytes=df-344", 545)

	// multiple byte ranges
	testParseByteRangeError(t, "bytes=1-2,4-6", 123)

	// byte range exceeding contentLength
	testParseByteRangeError(t, "bytes=123-", 12)

	// startPos exceeding endPos
	testParseByteRangeError(t, "bytes=123-34", 1234)
}

func testParseByteRangeError(t *testing.T, v string, contentLength int) {
	t.Helper()

	_, _, err := ParseByteRange([]byte(v), int64(contentLength))
	if err == nil {
		t.Fatalf("expecting error when parsing byte range %q", v)
	}
}

func TestFSCompressConcurrent(t *testing.T) {
	// Don't run this test on Windows, the Windows GitHub actions are too slow and timeout too often.
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	// This test can't run parallel as files in / might be changed by other tests.
	stop := make(chan struct{})
	defer close(stop)

	runFSCompressConcurrent(t, &FS{
		Root:               ".",
		GenerateIndexPages: true,
		Compress:           true,
		CleanStop:          stop,
	})
}

func TestFSCompressConcurrentSkipCache(t *testing.T) {
	// Don't run this test on Windows, the Windows GitHub actions are too slow and timeout too often.
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	// This test can't run parallel as files in / might be changed by other tests.
	stop := make(chan struct{})
	defer close(stop)

	runFSCompressConcurrent(t, &FS{
		Root:               ".",
		GenerateIndexPages: true,
		SkipCache:          true,
		Compress:           true,
		CleanStop:          stop,
	})
}

func runFSCompressConcurrent(t *testing.T, fs *FS) {
	t.Helper()

	h := fs.NewRequestHandler()

	concurrency := 4
	ch := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			for j := 0; j < 5; j++ {
				testFSCompress(t, h, "/fs.go")
				testFSCompress(t, h, "/examples/")
				testFSCompress(t, h, "/README.md")
			}
			ch <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second * 2000):
			t.Fatalf("timeout")
		}
	}
}

func TestFSCompressSingleThread(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSCompressSingleThread(t, &FS{
		Root:               ".",
		GenerateIndexPages: true,
		Compress:           true,
		CleanStop:          stop,
	})
}

func TestFSCompressSingleThreadSkipCache(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSCompressSingleThread(t, &FS{
		Root:               ".",
		GenerateIndexPages: true,
		SkipCache:          true,
		Compress:           true,
		CleanStop:          stop,
	})
}

func runFSCompressSingleThread(t *testing.T, fs *FS) {
	t.Helper()

	h := fs.NewRequestHandler()

	testFSCompress(t, h, "/fs.go")
	testFSCompress(t, h, "/")
	testFSCompress(t, h, "/README.md")
}

func testFSCompress(t *testing.T, h RequestHandler, filePath string) {
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

func TestFSHandlerSingleThread(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	requestHandler := FSHandler(".", 0)

	f, err := os.Open(".")
	if err != nil {
		t.Fatalf("cannot open cwd: %v", err)
	}

	filenames, err := f.Readdirnames(0)
	f.Close()
	if err != nil {
		t.Fatalf("cannot read dirnames in cwd: %v", err)
	}
	sort.Strings(filenames)

	for i := 0; i < 3; i++ {
		fsHandlerTest(t, requestHandler, filenames)
	}
}

func TestFSHandlerConcurrent(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	requestHandler := FSHandler(".", 0)

	f, err := os.Open(".")
	if err != nil {
		t.Fatalf("cannot open cwd: %v", err)
	}

	filenames, err := f.Readdirnames(0)
	f.Close()
	if err != nil {
		t.Fatalf("cannot read dirnames in cwd: %v", err)
	}
	sort.Strings(filenames)

	concurrency := 10
	ch := make(chan struct{}, concurrency)
	for j := 0; j < concurrency; j++ {
		go func() {
			for i := 0; i < 3; i++ {
				fsHandlerTest(t, requestHandler, filenames)
			}
			ch <- struct{}{}
		}()
	}

	for j := 0; j < concurrency; j++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}
}

func fsHandlerTest(t *testing.T, requestHandler RequestHandler, filenames []string) {
	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)
	ctx.Request.Header.SetHost("foobar.com")

	filesTested := 0
	for _, name := range filenames {
		f, err := os.Open(name)
		if err != nil {
			t.Fatalf("cannot open file %q: %v", name, err)
		}
		stat, err := f.Stat()
		if err != nil {
			t.Fatalf("cannot get file stat %q: %v", name, err)
		}
		if stat.IsDir() {
			f.Close()
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("cannot read file contents %q: %v", name, err)
		}

		ctx.URI().Update(name)
		requestHandler(&ctx)
		if ctx.Response.bodyStream == nil {
			t.Fatalf("response body stream must be non-empty")
		}
		body, err := io.ReadAll(ctx.Response.bodyStream)
		if err != nil {
			t.Fatalf("error when reading response body stream: %v", err)
		}
		if !bytes.Equal(body, data) {
			t.Fatalf("unexpected body returned: %q. Expecting %q", body, data)
		}
		filesTested++
		if filesTested >= 10 {
			break
		}
	}

	// verify index page generation
	ctx.URI().Update("/")
	requestHandler(&ctx)
	if ctx.Response.bodyStream == nil {
		t.Fatalf("response body stream must be non-empty")
	}
	body, err := io.ReadAll(ctx.Response.bodyStream)
	if err != nil {
		t.Fatalf("error when reading response body stream: %v", err)
	}
	if len(body) == 0 {
		t.Fatalf("index page must be non-empty")
	}
}

func TestStripPathSlashes(t *testing.T) {
	t.Parallel()

	testStripPathSlashes(t, "", 0, "")
	testStripPathSlashes(t, "", 10, "")
	testStripPathSlashes(t, "/", 0, "")
	testStripPathSlashes(t, "/", 1, "")
	testStripPathSlashes(t, "/", 10, "")
	testStripPathSlashes(t, "/foo/bar/baz", 0, "/foo/bar/baz")
	testStripPathSlashes(t, "/foo/bar/baz", 1, "/bar/baz")
	testStripPathSlashes(t, "/foo/bar/baz", 2, "/baz")
	testStripPathSlashes(t, "/foo/bar/baz", 3, "")
	testStripPathSlashes(t, "/foo/bar/baz", 10, "")

	// trailing slash
	testStripPathSlashes(t, "/foo/bar/", 0, "/foo/bar")
	testStripPathSlashes(t, "/foo/bar/", 1, "/bar")
	testStripPathSlashes(t, "/foo/bar/", 2, "")
	testStripPathSlashes(t, "/foo/bar/", 3, "")
}

func testStripPathSlashes(t *testing.T, path string, stripSlashes int, expectedPath string) {
	t.Helper()

	s := stripLeadingSlashes([]byte(path), stripSlashes)
	s = stripTrailingSlashes(s)
	if string(s) != expectedPath {
		t.Fatalf("unexpected path after stripping %q with stripSlashes=%d: %q. Expecting %q", path, stripSlashes, s, expectedPath)
	}
}

func TestFileExtension(t *testing.T) {
	t.Parallel()

	testFileExtension(t, "foo.bar", false, "zzz", ".bar")
	testFileExtension(t, "foobar", false, "zzz", "")
	testFileExtension(t, "foo.bar.baz", false, "zzz", ".baz")
	testFileExtension(t, "", false, "zzz", "")
	testFileExtension(t, "/a/b/c.d/efg.jpg", false, ".zzz", ".jpg")

	testFileExtension(t, "foo.bar", true, ".zzz", ".bar")
	testFileExtension(t, "foobar.zzz", true, ".zzz", "")
	testFileExtension(t, "foo.bar.baz.fasthttp.gz", true, ".fasthttp.gz", ".baz")
	testFileExtension(t, "", true, ".zzz", "")
	testFileExtension(t, "/a/b/c.d/efg.jpg.xxx", true, ".xxx", ".jpg")
}

func testFileExtension(t *testing.T, path string, compressed bool, compressedFileSuffix, expectedExt string) {
	t.Helper()

	ext := fileExtension(path, compressed, compressedFileSuffix)
	if ext != expectedExt {
		t.Fatalf("unexpected file extension for file %q: %q. Expecting %q", path, ext, expectedExt)
	}
}

func TestServeFileContentType(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	var ctx RequestCtx
	var req Request
	req.Header.SetMethod(MethodGet)
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFile(&ctx, "testdata/test.png")

	var resp Response
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []byte("image/png")
	if !bytes.Equal(resp.Header.ContentType(), expected) {
		t.Fatalf("Unexpected Content-Type, expected: %q got %q", expected, resp.Header.ContentType())
	}
}

func TestServeFileDirectoryRedirect(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("http://foobar.com")
	ctx.Init(&req, nil, nil)

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFile(&ctx, "fasthttputil")
	if ctx.Response.StatusCode() != StatusFound {
		t.Fatalf("Unexpected status code %d for directory '/fasthttputil' without trailing slash. Expecting %d.", ctx.Response.StatusCode(), StatusFound)
	}

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFile(&ctx, "fasthttputil/")
	if ctx.Response.StatusCode() != StatusOK {
		t.Fatalf("Unexpected status code %d for directory '/fasthttputil/' with trailing slash. Expecting %d.", ctx.Response.StatusCode(), StatusOK)
	}

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFile(&ctx, "fs.go")
	if ctx.Response.StatusCode() != StatusOK {
		t.Fatalf("Unexpected status code %d for file '/fs.go'. Expecting %d.", ctx.Response.StatusCode(), StatusOK)
	}
}

func TestZstDecompressFile(t *testing.T) {
	fs := FS{Root: ".", Compress: true, CacheDuration: time.Hour * 20}
	h := fs.NewRequestHandler()
	server := Server{
		Handler: h,
	}
	pcs := fasthttputil.NewPipeConns()
	cliCon, serCon := pcs.Conn1(), pcs.Conn2()
	//
	go func() {
		defer func() { cliCon.Close() }()
		req := AcquireRequest()
		resp := AcquireResponse()
		defer func() {
			ReleaseRequest(req)
			ReleaseResponse(resp)
		}()
		req.SetRequestURI("http://127.0.0.1:7070/fs.go")
		req.Header.Set(fasthttp.HeaderAcceptEncoding, "zstd")
		_, err := req.WriteTo(cliCon)
		if err != nil {
			t.Error(err)
			return
		}
		err = resp.Read(bufio.NewReader(cliCon))
		if err != nil {
			t.Error(err)
			return
		}
		if !bytes.Equal(resp.Header.ContentEncoding(), []byte("zstd")) {
			t.Error("not zstd")
			return
		}
		resp.Reset()
		req.Header.Del(fasthttp.HeaderAcceptEncoding)
		_, err = req.WriteTo(cliCon)
		if err != nil {
			t.Error(err)
			return
		}
		err = resp.Read(bufio.NewReader(cliCon))
		if err != nil {
			t.Error(err)
			return
		}
		if len(resp.Header.ContentEncoding()) != 0 {
			t.Error("has content encoding header")
			return
		}
		if utf8.Valid(resp.Body()) != true {
			t.Error("not valid utf8")
			return
		}
		err = cliCon.Close()
		if err != nil {
			t.Error(err)
		}
	}()
	err := server.ServeConn(serCon)
	//err := server.ListenAndServe(":7070")
	if err != nil {
		t.Fatal(err)
	}
}

func stripTrailingSlashes(path []byte) []byte {
	for len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path
}

func fileExtension(path string, compressed bool, compressedFileSuffix string) string {
	if compressed && strings.HasSuffix(path, compressedFileSuffix) {
		path = path[:len(path)-len(compressedFileSuffix)]
	}
	n := strings.LastIndexByte(path, '.')
	if n < 0 {
		return ""
	}
	return path[n:]
}

func TestCacheManagerSize(t *testing.T) {
	//cur, _ := os.Getwd()
	//f := cur + "/testdata2/vue.runtime.global.js"
	fs := FS{
		Root:          "testdata2",
		CacheDuration: time.Millisecond * 200,
		Compress:      true,
		Orders:        [3]compress.Order{compress.Br},
	}
	h := fs.NewRequestHandlerWithError()
	ctx := RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "br, gzip")
	ctx.Request.SetRequestURI("http://localhost/vue.runtime.global.js")
	err := h(&ctx)
	assert.Nil(t, err)
	assert.Equal(t, 1, fs.fh.cacheManager.(*syncMapCacheManager).caches[brotliCacheKind].Size())
	assert.Equal(t, ctx.Response.Header.ContentEncoding(), []byte("br"))
	//
	fs = FS{
		Root:          "testdata2",
		CacheDuration: time.Millisecond * 200,
		Compress:      true,
		Orders:        [3]compress.Order{compress.Gzip},
	}
	h = fs.NewRequestHandlerWithError()
	ctx.Request.Header.Set("Accept-Encoding", "gzip, br")
	ctx.Request.SetRequestURI("http://localhost/vue.runtime.global.js")
	h = fs.NewRequestHandlerWithError()
	err = h(&ctx)
	assert.Nil(t, err)
	assert.Equal(t, 1, fs.fh.cacheManager.(*syncMapCacheManager).caches[gzipCacheKind].Size())
	assert.Equal(t, ctx.Response.Header.ContentEncoding(), []byte("gzip"))
	//
	fs = FS{
		Root:          "testdata2",
		CacheDuration: time.Millisecond * 200,
		Compress:      true,
		Orders:        [3]compress.Order{compress.Zstd},
	}
	ctx.Request.Header.Set("Accept-Encoding", "zstd, gzip, br")
	ctx.Request.SetRequestURI("http://localhost/vue.runtime.global.js")
	h = fs.NewRequestHandlerWithError()
	err = h(&ctx)
	assert.Nil(t, err)
	assert.Equal(t, 1, fs.fh.cacheManager.(*syncMapCacheManager).caches[zstdCacheKind].Size())
	assert.Equal(t, ctx.Response.Header.ContentEncoding(), []byte("zstd"))
	//
	ctx.Request.Header.Del("Accept-Encoding")
	ctx.Request.SetRequestURI("http://localhost/vue.runtime.global.js")
	err = h(&ctx)
	assert.Nil(t, err)
	assert.Equal(t, 1, fs.fh.cacheManager.(*syncMapCacheManager).caches[0].Size())
	assert.Equal(t, ctx.Response.Header.ContentEncoding(), []byte("zstd"))
}

func TestPendingFiles(t *testing.T) {
	pfs := &pendingFiles{}
	pfs.unShift(&fileChain{})
	pfs.unShift(&fileChain{})
	pfs.unShift(&fileChain{})
	assert.Equal(t, 3, pfs.size())
	pfs.shift()
	assert.Equal(t, 2, pfs.size())
	pfs.shift()
	pfs.shift()
	pfs.shift()
	assert.Equal(t, 0, pfs.size())
}

var testSmallFilePool = &fsFilePool{
	sync.Pool{
		New: func() interface{} {
			return &fsFile{}
		},
	},
}

func TestHandlePendingCache(t *testing.T) {
	ca := syncMapCacheManager{}
	pfs := &pendingFiles{}
	idlePfs := &pendingFiles{}
	pfs.unShift(&fileChain{v: &fsFile{h: &fsHandler{
		filesystem: &osFS{}, smallFsFilePool: testSmallFilePool,
	}}})
	pfs.unShift(&fileChain{v: &fsFile{h: &fsHandler{
		filesystem: &osFS{}, smallFsFilePool: testSmallFilePool,
	}}})
	pfs.unShift(&fileChain{v: &fsFile{h: &fsHandler{
		filesystem: &osFS{}, smallFsFilePool: testSmallFilePool,
	}}})
	ca.handlePendingCache(pfs, idlePfs)
	ca.handlePendingCache(pfs, idlePfs)
	assert.Equal(t, 0, pfs.size())
	assert.Equal(t, 3, idlePfs.size())
}

func TestCleanCache(t *testing.T) {
	ca := syncMapCacheManager{
		caches: [4]*xsync.MapOf[string, *fsFile]{
			xsync.NewMapOf[string, *fsFile](),
			xsync.NewMapOf[string, *fsFile](),
			xsync.NewMapOf[string, *fsFile](),
			xsync.NewMapOf[string, *fsFile](),
		},
	}
	pfs := &pendingFiles{}
	idlePfs := &pendingFiles{}
	idlePfs.unShift(&fileChain{v: &fsFile{}})
	idlePfs.unShift(&fileChain{v: &fsFile{}})
	idlePfs.unShift(&fileChain{v: &fsFile{}})
	//
	ca.cleanCache(pfs, idlePfs)
	ca.cleanCache(pfs, idlePfs)
	assert.Equal(t, 0, pfs.size())
	assert.Equal(t, 3, idlePfs.size())
	//
	ca.caches[0].Store("f1", &fsFile{
		h:   &fsHandler{},
		dir: true,
	})
	ca.caches[0].Store("f2", &fsFile{
		h:   &fsHandler{},
		dir: true,
	})
	ca.caches[0].Store("f3", &fsFile{
		h:   &fsHandler{},
		dir: true,
	})
	ca.cleanCache(pfs, idlePfs)
	ca.cleanCache(pfs, idlePfs)
	assert.Equal(t, 3, idlePfs.size())
	assert.Equal(t, 0, pfs.size())
	ca.caches[0].Delete("f1")
	ca.caches[0].Delete("f2")
	ca.caches[0].Delete("f3")
	//
	ca.caches[0].Store("f1", &fsFile{
		h: &fsHandler{
			filesystem: &osFS{},
		},
		dir: true,
	})
	ca.caches[0].Store("f2", &fsFile{
		h:   &fsHandler{},
		dir: true,
	})
	ca.caches[0].Store("f3", &fsFile{
		h:   &fsHandler{},
		dir: true,
	})
	ca.caches[0].Store("f4", &fsFile{
		h: &fsHandler{
			filesystem: &osFS{},
		},
		// delete
		dir: true,
	})
	ca.caches[0].Store("f5", &fsFile{
		h: &fsHandler{
			filesystem: &osFS{},
		},
		// open error
		// delete
		originalFileName: []byte("f4"),
	})

	cur, _ := os.Getwd()
	f := cur + "/testdata2/vue.runtime.global.js"
	ca.caches[0].Store("f6", &fsFile{
		h: &fsHandler{
			filesystem: &osFS{},
		},
		// file stale
		// delete
		originalFileName: []byte(f),
	})
	//
	f = cur + "/testdata2/index.html"
	ca.caches[0].Store("f7", &fsFile{
		h: &fsHandler{
			filesystem: &osFS{},
		},
		// file is fresh
		lastModified:     time.Now(),
		originalFileName: []byte(f),
	})
	//

	idlePfs.unShift(&fileChain{v: &fsFile{}})
	idlePfs.unShift(&fileChain{v: &fsFile{}})
	idlePfs.unShift(&fileChain{v: &fsFile{}})
	idlePfs.unShift(&fileChain{v: &fsFile{}})
	idlePfs.unShift(&fileChain{v: &fsFile{}})
	//
	size := idlePfs.size()
	ca.cleanCache(pfs, idlePfs)
	ca.cleanCache(pfs, idlePfs)
	assert.Equal(t, 4, pfs.size())
	assert.Equal(t, size-4, idlePfs.size())
	assert.Equal(t, 3, ca.caches[0].Size())
}

func TestContentType(t *testing.T) {
	fs := &FS{Compress: true,
		Root: "testdata2",
	}
	h := fs.NewRequestHandlerWithError()
	ctx := RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "gzip")
	ctx.Request.SetRequestURI("http://127.0.0.1/vue.runtime.global.js")
	err := h(&ctx)
	assert.Nil(t, err)
	assert.Equal(t, []byte("application/javascript"), ctx.Response.Header.ContentType())
	//
	ctx.Response.Header.Reset()
	ctx.Request.SetRequestURI("http://127.0.0.1/index.html")
	err = h(&ctx)
	log.Println(string(ctx.Response.Body()))
	log.Println(string(ctx.Response.Header.ContentType()))
	assert.Equal(t, []byte("text/html"), ctx.Response.Header.ContentType())
}
