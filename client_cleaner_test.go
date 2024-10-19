package fasthttp

import (
	"testing"
	"time"
)

func TestClient_Clean(t *testing.T) {
	t.Skip()
	cli := &Client{}
	req := AcquireRequest()
	resp := AcquireResponse()
	defer func() {
		ReleaseRequest(req)
		ReleaseResponse(resp)
	}()
	var hc *HostClient
	for i := 0; i < 10; i++ {
		req.SetRequestURI(`https://www.baidu.com`)
		err := cli.Do(req, resp)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode() != 200 {
			t.Fatalf("unexpected status code: %d. Expecting 200", resp.StatusCode())
		}
		cli.mLock.Lock()
		hc = cli.ms[`www.baidu.com`]
		cli.mLock.Unlock()
	}
	time.Sleep(time.Second * 25)

	if hc == nil {
		t.Fatalf("hc is nil")
	}
	if len(cli.ms) != 0 || hc.connsCount != 0 {
		t.Fatalf("len(cli.ms) = %d. Expecting 0", len(cli.ms))
	}
	l := len(defaultClientCleaner.items)
	if l != 0 {
		t.Fatalf("len(defaultClientCleaner.items) = %d. Expecting 0", l)
	}
}
